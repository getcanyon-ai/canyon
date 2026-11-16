package runs

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/sarif"
	"github.com/getcanyon-ai/canyon/vault"
)

// Two first attaches race for the project's report key: the second waits on the first's row lock and
// answers the key the first sealed, which is stored sealed, never as itself, and read back the same.
func TestTheReportKeyIsGeneratedOnceAndSealed(t *testing.T) {
	p, ctx := seeded(t)
	keys, err := vault.Load(filepath.Join(t.TempDir(), "kek"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Rollback(ctx) }()
	key, err := ReportKey(ctx, first, keys, project)
	if err != nil || len(key) != 32 {
		t.Fatalf("the first call: %d bytes, %v", len(key), err)
	}
	second, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Rollback(ctx) }()
	var pid int
	if err := second.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		key []byte
		err error
	}
	done := make(chan answer, 1)
	go func() { k, err := ReportKey(ctx, second, keys, project); done <- answer{k, err} }()
	for waiting := false; !waiting; {
		if len(done) > 0 {
			t.Fatalf("the second call returned before the first committed: %+v", <-done)
		}
		if err := p.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1 AND wait_event_type = 'Lock')", pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := <-done; got.err != nil || !bytes.Equal(got.key, key) {
		t.Fatalf("the second call answers the first's key: equal %v, %v", bytes.Equal(got.key, key), got.err)
	}
	if err := second.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var sealedKey, sealedValue []byte
	if err := p.QueryRow(ctx, "SELECT report_sealed_key, report_sealed_value FROM projects WHERE id = $1", project).Scan(&sealedKey, &sealedValue); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealedKey, key) || bytes.Contains(sealedValue, key) || len(sealedValue) != len(key)+vault.Overhead {
		t.Fatalf("the key is stored sealed: %d bytes", len(sealedValue))
	}
	third, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = third.Rollback(ctx) }()
	if again, err := ReportKey(ctx, third, keys, project); err != nil || !bytes.Equal(again, key) {
		t.Fatalf("a later call reads the same key: %v", err)
	}
}

// A deleted project's report key went with its secrets, and no attach mints another.
func TestADeletedProjectGetsNoReportKey(t *testing.T) {
	p, ctx := seeded(t)
	keys, err := vault.Load(filepath.Join(t.TempDir(), "kek"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "UPDATE projects SET deleted_at = now()"); err != nil {
		t.Fatal(err)
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := ReportKey(ctx, tx, keys, project); err == nil {
		t.Fatal("a deleted project answered a report key")
	}
	var written bool // read in the caller's transaction, before anything rolls it back
	if err := tx.QueryRow(ctx, "SELECT report_sealed_value IS NOT NULL FROM projects").Scan(&written); err != nil || written {
		t.Fatalf("a key was written for the deleted project: %v %v", written, err)
	}
}

// Every attach is a row; the package's current report per (tool, category) is the newest attach,
// the same report attached again after another included, and a run reads back by id exactly what
// was attached, its results without partialFingerprints.
func TestTheCurrentReportIsTheNewestAttach(t *testing.T) {
	p, ctx := seeded(t)
	raw, err := os.ReadFile("../contracts/testdata/finding/imported-semgrep.sarif.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden finding.Log
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	report := func(tool, category, digest, by string) Report {
		return Report{Report: sarif.Report{Tool: tool, Category: category, Version: "1.86.0", Results: golden.Runs[0].Results, Dropped: 2, FirstDropped: "result 3: kind is pass"},
			Digest: "sha256:" + digest, AttachedBy: events.Actor{Kind: "agent", Subject: by, Display: by}}
	}
	a, b := report("semgrep", "", strings.Repeat("a", 64), "ci-bot"), report("semgrep", "", strings.Repeat("b", 64), "dana")
	b.Dropped, b.FirstDropped = 0, ""
	codeql := report("codeql", "js", strings.Repeat("c", 64), "ci-bot")
	var ids []int64
	var replaced []string
	for _, r := range []Report{a, b, a, codeql} {
		tx, err := p.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := events.LockTx(ctx, tx); err != nil {
			t.Fatal(err)
		}
		id, was, err := AttachReportTx(ctx, tx, project, pkg, r)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		ids, replaced = append(ids, id), append(replaced, was)
	}
	if want := []string{"", a.Digest, b.Digest, ""}; !slices.Equal(replaced, want) {
		t.Fatalf("replaced %q, want %q", replaced, want)
	}
	current, err := CurrentReports(ctx, p, pkg, []string{"codeql", "semgrep"})
	if err != nil || len(current) != 2 || current[0] != ids[3] || current[1] != ids[2] {
		t.Fatalf("current %v (%v); want codeql's attach, then semgrep's newest of %v", current, err, ids)
	}
	got, err := ReadReports(ctx, p, pkg, []int64{ids[1], ids[0]})
	if err != nil {
		t.Fatal(err)
	}
	want := []Report{a, b} // one tool, one category: in attach order
	stripped := make([]finding.Result, len(golden.Runs[0].Results))
	copy(stripped, golden.Runs[0].Results)
	for i := range stripped {
		stripped[i].PartialFingerprints = finding.Fingerprints{}
	}
	for i := range want {
		want[i].Results = stripped
		if g, w := mustJSON(t, got[i]), mustJSON(t, want[i]); g != w {
			t.Fatalf("report %d read back\n%s\nwant\n%s", i, g, w)
		}
	}
	var fingerprinted bool
	if err := p.QueryRow(ctx, "SELECT bool_or(r ? 'partialFingerprints') FROM package_reports, jsonb_array_elements(results) r").Scan(&fingerprinted); err != nil || fingerprinted {
		t.Fatalf("a stored result carries partialFingerprints: %v %v", fingerprinted, err)
	}
	if _, err := ReadReports(ctx, p, "pkg_other", []int64{ids[0]}); err == nil {
		t.Fatal("another package's read of this package's report answered")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
