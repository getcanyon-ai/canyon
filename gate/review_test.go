package gate

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/getcanyon-ai/canyon/contracts"
	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/policy"
)

const claimsPortal = "testdata/apps/claims-portal"

const recordedModel = "recorded-model"

// model is a test double: it records every request it is sent and answers with resp or err.
type model struct {
	resp  Response
	err   error
	calls []Request
	mu    sync.Mutex // a split review's parts call at once
}

func (m *model) Review(_ context.Context, req Request) (Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, req)
	return m.resp, m.err
}

// golden is the log the recording reproduces (testdata/review) and scanned the seam-1 golden's
// scanners' run alone: what the gate hands the reviewer over the fixture app.
func golden(t *testing.T) (golden, scanned finding.Log) {
	t.Helper()
	raw, err := os.ReadFile("testdata/review/claims-portal.sarif.json")
	if err := cmp.Or(err, json.Unmarshal(raw, &golden), json.Unmarshal(contracts.GoldenLog, &scanned)); err != nil {
		t.Fatal(err)
	}
	scanned.Runs = scanned.Runs[:1:1]
	return golden, scanned
}

func reviewer(t *testing.T, m Model) *Reviewer {
	snap, err := (&policy.Builder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &Reviewer{Snapshot: snap, Agent: "canyon-review", ModelID: recordedModel, Model: m}
}

// plant writes each file under dir.
func plant(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for f, text := range files {
		if err := cmp.Or(os.MkdirAll(filepath.Join(dir, filepath.Dir(f)), 0o700), os.WriteFile(filepath.Join(dir, f), []byte(text), 0o600)); err != nil {
			t.Fatal(err)
		}
	}
}

// unleaked fails if any 8 consecutive characters of a secret reach the request.
func unleaked(t *testing.T, req Request, secrets ...string) {
	t.Helper()
	sent, _ := json.Marshal(req)
	for _, secret := range secrets {
		r := []rune(secret)
		for i := 0; i+8 <= len(r); i++ {
			if strings.Contains(string(sent), string(r[i:i+8])) {
				t.Fatalf("8 characters of a secret reached the model's request: %.4s…", string(r[i:i+8]))
			}
		}
	}
}

// fake is n random-looking alphanumerics made at run time, so no credential sits in the repo for its own scan.
func fake(seed string, n int) string {
	sum := sha256.Sum256([]byte(seed))
	s := strings.Map(func(r rune) rune {
		if r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			return r
		}
		return -1
	}, base64.StdEncoding.EncodeToString(append(sum[:], sum[:]...)))
	return s[:n]
}

// Server-side credentials no Canyon scanner flags: an AWS secret access key, a GitHub token, a PEM key.
func credentials() (aws, github, pem string) {
	body := fake("pem-1", 60) + "\n" + fake("pem-2", 60) + "\n" + fake("pem-3", 60)
	return fake("aws", 40), "ghp_" + fake("github", 36), "-----BEGIN RSA PRIV" + "ATE KEY-----\n" + body + "\n-----END RSA PRIV" + "ATE KEY-----"
}

// Replay over the fixture app reproduces the recorded canyon-review run (two routes, ClaimsTable.tsx:61),
// labeled recorded, and the scanners' run comes back byte for byte.
func TestReplayReproducesTheGoldenAIReviewRow(t *testing.T) {
	want, scanned := golden(t)
	var replay Replay
	raw, err := os.ReadFile("testdata/review/claims-portal.json")
	if err := cmp.Or(err, json.Unmarshal(raw, &replay)); err != nil {
		t.Fatal(err)
	}
	r := reviewer(t, replay)
	before, _ := json.Marshal(scanned)
	out, err := r.Review(context.Background(), claimsPortal, scanned)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(scanned)
	kept, _ := json.Marshal(finding.Log{Version: out.Version, Schema: out.Schema, Runs: out.Runs[:1]})
	if len(out.Runs) != 2 || string(after) != string(before) || string(kept) != string(before) {
		t.Fatalf("the reviewer adds its own run and leaves the scanners' as they were: %d runs", len(out.Runs))
	}
	mustSchemas(t, out)
	got := out.Runs[1]
	if len(got.Results) != 3 || slices.ContainsFunc(got.Results, func(r finding.Result) bool { return !r.Properties.Canyon.Recorded }) {
		t.Fatalf("a replayed row is labeled recorded in the row itself: %+v", got.Results)
	}
	if inv := got.Invocations; len(inv) != 1 || len(inv[0].ToolExecutionNotifications) != 2 ||
		inv[0].ToolExecutionNotifications[0].Message.Text != "ai review · replayed from a recording" {
		t.Fatalf("a replayed run says so even with no results: %+v", inv)
	}
	// The golden predates the labels, and its blocks is the server's; the rest is exact.
	for i := range got.Results {
		got.Results[i].Properties.Canyon.Recorded = false
	}
	got.Invocations = nil
	derived := finding.Log{Version: finding.Version, Schema: finding.SchemaURI, Runs: []finding.Run{got}}
	if err := finding.Derive(&derived, r.Snapshot); err != nil {
		t.Fatal(err)
	}
	gotJSON, _ := json.Marshal(derived.Runs[0])
	wantJSON, _ := json.Marshal(want.Runs[1])
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("the replayed run is not the golden's:\n got %s\nwant %s", gotJSON, wantJSON)
	}
}

// No model: the reviewer's run is there with D10's string and no results — visibly skipped.
func TestWithoutAModelTheReviewIsVisiblySkipped(t *testing.T) {
	_, scanned := golden(t)
	out, err := reviewer(t, nil).Review(context.Background(), claimsPortal, scanned)
	if err != nil {
		t.Fatal(err)
	}
	mustSchemas(t, out)
	run := out.Runs[len(out.Runs)-1]
	if len(out.Runs) != 2 || run.Tool.Driver.Name != "canyon-review" || len(run.Results) != 0 {
		t.Fatalf("the skipped reviewer still has its run, with no results: %+v", out.Runs)
	}
	inv := run.Invocations
	if len(inv) != 1 || len(inv[0].ToolExecutionNotifications) != 1 ||
		inv[0].ToolExecutionNotifications[0].Message.Text != "ai review · skipped (no model configured on this Canyon host)" {
		t.Fatalf("the run carries D10's skipped string: %+v", inv)
	}
	var fixture finding.Log
	raw, err := os.ReadFile("../contracts/testdata/finding/skipped-review.sarif.json")
	if err := cmp.Or(err, json.Unmarshal(raw, &fixture)); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(run)
	want, _ := json.Marshal(fixture.Runs[0])
	if string(got) != string(want) {
		t.Fatalf("the skipped run is not the contract fixture's:\n got %s\nwant %s", got, want)
	}
}

// A replay with no results is still labeled: the run says it was replayed from a recording.
func TestAnEmptyReplayIsLabeled(t *testing.T) {
	dir := t.TempDir()
	plant(t, dir, map[string]string{"app.ts": "export {};\n"})
	out, err := reviewer(t, &model{resp: Response{Recorded: true}}).Review(context.Background(), dir, finding.Log{})
	if err != nil {
		t.Fatal(err)
	}
	mustSchemas(t, out)
	inv := out.Runs[0].Invocations
	if len(inv) != 1 || len(inv[0].ToolExecutionNotifications) != 2 ||
		inv[0].ToolExecutionNotifications[0].Message.Text != "ai review · replayed from a recording" {
		t.Fatalf("an empty replay is labeled: %+v", inv)
	}
}

// Source files only, keys masked: the Stripe key — gitleaks finds it, payments.ts:14 columns 27–63
// as the golden places it — a .env password and a config token never reach the model.
func TestTheModelIsShownMaskedSourceOnly(t *testing.T) {
	_, scanned := golden(t)
	region := finding.Region{StartLine: 14, StartColumn: 27, EndColumn: 63}
	dir, password, token := t.TempDir(), "hunter2-"+"db-pass", "tok-"+"cfg-9f8e7d6c"
	if err := os.CopyFS(dir, os.DirFS(claimsPortal)); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join(claimsPortal, "apps/web/src/lib/payments.ts"))
	if err != nil {
		t.Fatal(err)
	}
	literal := string([]rune(strings.Split(string(src), "\n")[region.StartLine-1])[region.StartColumn : region.EndColumn-2]) // inside the quotes
	plant(t, dir, map[string]string{".env.local": "DB_PASSWORD=" + password, "config.json": `{"token": "` + token + `"}`,
		"node_modules/x.js": password, ".git/x.js": password, ".env.ts": password, "apps/web/.env.js": password,
		"apps/api/src/billing.ts": "export const fallback = '" + literal + "';\n"})
	m := &model{}
	if _, err := reviewer(t, m).Review(context.Background(), dir, scanned); err != nil {
		t.Fatal(err)
	}
	if len(m.calls) != 1 {
		t.Fatalf("the model is called once: %d", len(m.calls))
	}
	unleaked(t, m.calls[0], literal, password, token)
	if !strings.Contains(m.calls[0].Files["apps/api/src/billing.ts"], "'••••'") {
		t.Fatal("the key is masked in every shown file it appears in")
	}
	shown := m.calls[0].Files["apps/web/src/components/ClaimsTable.tsx"]
	if !strings.Contains(shown, "useEffect(() => { refetch(); }, [query]);") || m.calls[0].Model != recordedModel {
		t.Fatal("the request carries the source in scope and the resolved model id")
	}
}

// A live key in a shown file reaches the model masked, whatever flagged it or did not: the reviewer
// masks what gitleaks finds, and the backstop finds nothing left (contracts/finding.md, a live key).
func TestALiveKeyNothingFlaggedReachesTheModelMasked(t *testing.T) {
	dir, key := t.TempDir(), "sk_"+"live_"+fake("live", 24)
	if err := os.WriteFile(filepath.Join(dir, "app.ts"), []byte(`const k = "`+key+`";`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &model{}
	if _, err := reviewer(t, m).Review(context.Background(), dir, finding.Log{}); err != nil || len(m.calls) != 1 {
		t.Fatalf("the review: err %v, %d calls", err, len(m.calls))
	}
	unleaked(t, m.calls[0], key)
}

// Credentials no Canyon scanner flags are gitleaks' to find: each is masked wherever it appears in a
// shown file by bullets alone, a PEM key keeping its line breaks so line numbers hold, a URL keeping
// all but its password, a gitleaks:allow comment or not.
func TestCredentialsNoScannerFlaggedAreMaskedBeforeTheModel(t *testing.T) {
	aws, github, pem := credentials()
	dir := t.TempDir()
	password := fake("db", 20)
	server := "const awsSecretAccessKey = \"" + aws + "\"; // gitleaks:allow\nconst gh = \"" + github + "\";\nconst key = `" + pem + "`;\n" +
		"const db = \"postgres://app:" + password + "@db.internal:5432/claims\";\nexport {};\n"
	plant(t, dir, map[string]string{"apps/api/src/server.ts": server, "apps/api/src/again.ts": "// " + github + "\n"})
	m := &model{}
	if _, err := reviewer(t, m).Review(context.Background(), dir, finding.Log{}); err != nil {
		t.Fatal(err)
	}
	unleaked(t, m.calls[0], aws, github, pem, password)
	shown := m.calls[0].Files["apps/api/src/server.ts"]
	if !strings.Contains(shown, `const gh = "••••";`) || m.calls[0].Files["apps/api/src/again.ts"] != "// ••••\n" ||
		!strings.Contains(shown, `"postgres://app:••••@db.internal:5432/claims"`) || strings.Count(shown, "\n") != strings.Count(server, "\n") {
		t.Fatalf("each credential is masked where it stood, line breaks kept:\n%s", shown)
	}
}

// Every literal gitleaks finds is masked in every shown file, the longest first: a key that is the start of
// a longer one never masks that one's start and leaves its tail, and a PEM key keeps its line breaks (#2279).
func TestTheLongestLiteralIsMaskedFirst(t *testing.T) {
	_, _, pem := credentials()
	short := fake("short", 24)
	long := short + fake("long", 12)
	dir := t.TempDir()
	plant(t, dir, map[string]string{
		"a.ts": "const api_key = \"" + short + "\";\nconst secret_key = \"" + long + "\";\nconst pem = `" + pem + "`;\n",
		"b.ts": "// " + long + " " + short + " " + long + "\n",
	})
	_, shown, _, err := excerpts(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"a.ts": "const api_key = \"••••\";\nconst secret_key = \"••••\";\nconst pem = `••••\n\n\n\n`;\n",
		"b.ts": "// •••• •••• ••••\n",
	}
	if !maps.Equal(shown, want) {
		t.Fatalf("shown %q, want %q", shown, want)
	}
}

// Two keys where one starts inside the other are masked as one, whichever is longer: neither the first's
// start nor the second's tail reaches the model (#2279).
func TestOverlappingLiteralsAreMaskedWhole(t *testing.T) {
	short := fake("short", 24)
	long := short[12:] + fake("tail", 36)
	dir := t.TempDir()
	plant(t, dir, map[string]string{"a.ts": "const api_key = \"" + short + "\";\nconst secret_key = \"" + long + "\";\n// " + short[:12] + long + "\n"})
	_, shown, _, err := excerpts(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := "const api_key = \"••••\";\nconst secret_key = \"••••\";\n// ••••\n"; shown["a.ts"] != want {
		t.Fatalf("shown %q, want %q", shown["a.ts"], want)
	}
}

// A password found on every one of thousands of lines is one literal to mask, not one per finding: each
// copy found every other's place, and masking took memory growing with the square of the lines (#2279).
func TestARepeatedLiteralIsMaskedOnce(t *testing.T) {
	password := fake("db", 20)
	line := "const db = \"postgres://app:" + password + "@db.internal:5432/claims\";\n"
	dir := t.TempDir()
	plant(t, dir, map[string]string{"a.ts": strings.Repeat(line, 4000)})
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, shown, _, err := excerpts(dir)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Repeat(strings.ReplaceAll(line, password, "••••"), 4000); shown["a.ts"] != want {
		t.Fatal("every copy is masked")
	}
	// TotalAlloc is the whole process's, gitleaks included: CI measured 109 MiB, and the quadratic bug allocated 1277 MiB
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 512<<20 {
		t.Fatalf("masking 4000 copies of one password allocated %d MiB", grew>>20)
	}
}

// The reviewer reads every common language and the templates that render pages, beside JS and TS: a
// Flask route, a Go handler, a Rails view and the rest reach the model, masked as JS is; a README, a
// lockfile, an .env and a Gradle build script do not.
func TestTheReviewerReadsEveryCommonLanguage(t *testing.T) {
	_, github, _ := credentials()
	shown := []string{"app.py", "main.go", "Api.java", "Api.kt", "users_controller.rb", "index.php", "Api.cs", "main.rs",
		"Api.scala", "App.swift", "templates/index.html", "page.htm", "show.html.erb", "base.jinja", "base.jinja2", "base.j2", "App.vue", "Page.svelte", "a.ts"}
	files := map[string]string{"README.md": "x\n", "requirements.txt": "flask\n", ".env": "K=v\n", "build.gradle.kts": "plugins { kotlin(\"jvm\") }\n"}
	for _, f := range shown {
		files[f] = "token = \"" + github + "\"\n"
	}
	files["main.rs"] = "fn main() {\n    let token = \"" + github + "\";\n}\n" // .rs is Rust, RenderScript or XML: Linguist reads which
	dir := t.TempDir()
	plant(t, dir, files)
	m := &model{}
	if _, err := reviewer(t, m).Review(context.Background(), dir, finding.Log{}); err != nil {
		t.Fatal(err)
	}
	for _, f := range shown {
		if got, ok := m.calls[0].Files[f]; !ok || got != strings.ReplaceAll(files[f], github, "••••") {
			t.Errorf("%s: shown %v as %q", f, ok, got)
		}
	}
	if len(m.calls[0].Files) != len(shown) {
		t.Errorf("shown %d files, want %d: a README, a lockfile, an .env and a build script are not source", len(m.calls[0].Files), len(shown))
	}
}

// Over 16 MiB of shown source the model is not called and the run goes on, the reviewer's run saying why;
// files never shown are neither read nor counted.
func TestSourceOverTheCapIsNotCheckedByName(t *testing.T) {
	dir := t.TempDir()
	plant(t, dir, map[string]string{"app.ts": "export {};\n", "node_modules/big.js": strings.Repeat("x", 3<<20), "secret.txt": "x"})
	if err := os.Chmod(filepath.Join(dir, "secret.txt"), 0); err != nil {
		t.Fatal(err)
	}
	m := &model{}
	if _, err := reviewer(t, m).Review(context.Background(), dir, finding.Log{}); err != nil || len(m.calls) != 1 {
		t.Fatalf("files never shown are not read or counted: %v", err)
	}
	plant(t, dir, map[string]string{"big.ts": strings.Repeat("x", 16<<20)})
	out, err := reviewer(t, m).Review(context.Background(), dir, finding.Log{})
	if err != nil || len(m.calls) != 1 {
		t.Fatalf("over the ceiling the model is not called and the run goes on: %v, %d call(s)", err, len(m.calls))
	}
	if run := out.Runs[len(out.Runs)-1]; len(run.Results) != 0 || run.Invocations[0].ToolExecutionNotifications[0].Message.Text != TooLarge || Unread(run) != TooLarge {
		t.Fatalf("the reviewer's run says why it read nothing: %+v", run)
	}
}

// split is a model answering each call of a split review with one result on the first file of its part
// and one on a file of another part no other result names, which the review must drop: a call names only
// its own part's files.
type split struct {
	mu    sync.Mutex
	calls []Request
	room  bool // the third part's answer runs out of room
}

func (m *split) Review(_ context.Context, req Request) (Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, req)
	files := slices.Sorted(maps.Keys(req.Files))
	if m.room && files[0] == "c/5.ts" {
		return Response{}, fmt.Errorf("%w: stopped at max_tokens", ErrOutOfRoom)
	}
	others := slices.DeleteFunc(slices.Clone(req.Paths), func(p string) bool { _, ok := req.Files[p]; return ok || p == "c/5.ts" })
	at := func(file string) Candidate {
		return Candidate{Rule: "perf.unbounded-refetch", File: file, StartLine: 1, EndLine: 1, Severity: "medium", Title: "Refetch", Problem: "p", Fix: "f"}
	}
	resp := Response{Results: []Candidate{at(files[0])}}
	if len(others) > 0 {
		resp.Results = append(resp.Results, at(others[len(others)-1])) // a/2.ts or b/4.ts: never a file another part's own result names
	}
	return resp, nil
}

// splitApp is five files of three folders, and the room that puts two in a call: three parts.
func splitApp(t *testing.T) (string, int) {
	t.Helper()
	dir, files := t.TempDir(), map[string]string{}
	for i, name := range []string{"a/1.ts", "a/2.ts", "b/3.ts", "b/4.ts", "c/5.ts"} {
		files[name] = fmt.Sprintf("export const f%d = () => fetch('/api/%d');\n", i, i)
	}
	plant(t, dir, files)
	order := slices.Sorted(maps.Keys(files))
	return dir, len(pathsBlock(order)) + len(fileBlock("a/1.ts", files["a/1.ts"]))*2
}

// Source more than one call takes is split in path order, each call sent every path and its part's files;
// the parts' results are one run, a result on another part's file dropped and counted, and the run says
// how many parts there were (contracts/finding.md, #2274).
func TestTheReviewSplitsAcrossCalls(t *testing.T) {
	dir, room := splitApp(t)
	m := &split{}
	r := reviewer(t, m)
	r.room = room
	out, err := r.Review(context.Background(), dir, finding.Log{})
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(m.calls, func(x, y Request) int {
		return strings.Compare(slices.Min(slices.Collect(maps.Keys(x.Files))), slices.Min(slices.Collect(maps.Keys(y.Files))))
	})
	var parts [][]string
	for _, c := range m.calls {
		parts = append(parts, slices.Sorted(maps.Keys(c.Files)))
		if !slices.Equal(c.Paths, []string{"a/1.ts", "a/2.ts", "b/3.ts", "b/4.ts", "c/5.ts"}) {
			t.Errorf("a call is sent every path: %q", c.Paths)
		}
	}
	if fmt.Sprint(parts) != "[[a/1.ts a/2.ts] [b/3.ts b/4.ts] [c/5.ts]]" {
		t.Fatalf("parts in path order, a folder's files together: %v", parts)
	}
	run := out.Runs[len(out.Runs)-1]
	var got []string
	for _, res := range run.Results {
		got = append(got, res.Locations[0].PhysicalLocation.ArtifactLocation.URI)
	}
	if fmt.Sprint(got) != "[a/1.ts b/3.ts c/5.ts]" || Unread(run) != "" {
		t.Fatalf("one run of every part's results on its own files: %v, unread %q", got, Unread(run))
	}
	var notes []string
	for _, n := range run.Invocations[0].ToolExecutionNotifications {
		notes = append(notes, n.Message.Text)
	}
	if !slices.Contains(notes, "ai review · reviewed in 3 parts: the source is more than one model call takes") || !slices.Contains(notes, fmt.Sprintf(Dropped, 3)) {
		t.Fatalf("the run says it was split and what it dropped: %q", notes)
	}
	mustSchemas(t, out)
}

// One part out of room is the whole review's: no result, the run's note, as one call's (contracts/finding.md).
func TestAPartOutOfRoomIsTheWholeReviews(t *testing.T) {
	dir, room := splitApp(t)
	r := reviewer(t, &split{room: true})
	r.room = room
	out, err := r.Review(context.Background(), dir, finding.Log{})
	if run := out.Runs[len(out.Runs)-1]; err != nil || len(run.Results) != 0 || Unread(run) != OutOfRoom {
		t.Fatalf("a part out of room: %v, %+v", err, run)
	}
}

// A file that alone is more than a call takes is shown to none: the run names it and reads as one that
// read nothing, whatever the other calls found (contracts/finding.md, #2274).
func TestAFileOverTheCallBudgetIsNamedAndUnread(t *testing.T) {
	dir, room := splitApp(t)
	plant(t, dir, map[string]string{"b/huge.ts": strings.Repeat("// x\n", room)})
	m := &split{}
	r := reviewer(t, m)
	r.room = room
	out, err := r.Review(context.Background(), dir, finding.Log{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range m.calls {
		if _, shown := c.Files["b/huge.ts"]; shown || !slices.Contains(c.Paths, "b/huge.ts") {
			t.Fatalf("the file is listed and never shown: %q", slices.Sorted(maps.Keys(c.Files)))
		}
	}
	run := out.Runs[len(out.Runs)-1]
	warned := slices.ContainsFunc(run.Invocations[0].ToolExecutionNotifications, func(n finding.Notification) bool {
		return n.Level == "warning" && n.Message.Text == `ai review · not checked: too large for one AI review call: "b/huge.ts"`
	})
	if !warned || Unread(run) != TooLargeForACall || len(run.Results) == 0 {
		t.Fatalf("the run names the file and reads as unread, the others' results kept: %+v", run.Invocations)
	}
	r.Agent = CustomRules
	r.Snapshot.Agents = append(r.Snapshot.Agents, policy.Agent{ID: CustomRules, Namespace: []string{"custom"}})
	r.Snapshot.Rules = append(r.Snapshot.Rules, policy.Rule{ID: "custom.x", Name: "x", State: policy.Advisory, Producer: policy.LLMReview, Instructions: "x"})
	r.Model = &model{resp: Response{Results: []Candidate{}}}
	if out, err = r.Review(context.Background(), dir, finding.Log{}); err != nil || Unread(out.Runs[len(out.Runs)-1]) != TooLargeForACall {
		t.Fatalf("the custom rules' run reads as unread too: %v", err)
	}
}

// A file the reviewer would be shown that is not valid UTF-8 — a Latin-1 Ruby file, a stray 0xFF byte in a
// comment — is never sent and is named, the first 20 by path and the rest counted, and the run reads as one
// that read nothing, whatever the other files' results (contracts/finding.md; #2280: it was skipped unsaid).
func TestAFileNotValidUTF8IsNamedAndUnread(t *testing.T) {
	dir := t.TempDir()
	tree := map[string]string{"app.ts": "export const ok = 1;\n", "lib/legacy.rb": "# caf\xe9\nclass Legacy\nend\n", "web/main.js": "// \xff\nexport {};\n"}
	for i := range 20 {
		tree[fmt.Sprintf("old/f%02d.py", i)] = "# \xe9\n"
	}
	plant(t, dir, tree)
	m := &model{resp: Response{Results: []Candidate{{Rule: "perf.unbounded-refetch", File: "app.ts", StartLine: 1, EndLine: 1, Severity: "high", Title: "t", Problem: "p", Fix: "f"}}}}
	r := reviewer(t, m)
	out, err := r.Review(context.Background(), dir, finding.Log{})
	if err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(maps.Keys(m.calls[0].Files)); !slices.Equal(got, []string{"app.ts"}) {
		t.Fatalf("shown %q: a file not valid UTF-8 is never sent", got)
	}
	run := out.Runs[len(out.Runs)-1]
	var warned []string
	for _, n := range run.Invocations[0].ToolExecutionNotifications {
		if n.Level == "warning" {
			warned = append(warned, n.Message.Text)
		}
	}
	if len(warned) != 21 || warned[0] != `ai review · not checked: not valid UTF-8: "lib/legacy.rb"` || warned[1] != `ai review · not checked: not valid UTF-8: "old/f00.py"` ||
		warned[19] != `ai review · not checked: not valid UTF-8: "old/f18.py"` || warned[20] != "ai review · not checked: 2 more files not valid UTF-8" {
		t.Fatalf("the first 20 by path, then the rest counted: %q", warned)
	}
	if Unread(run) != NotUTF8 || len(run.Results) != 1 {
		t.Fatalf("the run reads as unread, its results kept: %q, %d", Unread(run), len(run.Results))
	}
}

// Only a dotenv file — .env, .env.<anything> — is hidden as one: .envoy.ts and .environment.js are code the
// reviewer is shown (contracts/finding.md; #2280: every name starting .env was hidden).
func TestOnlyADotenvFileIsHiddenAsOne(t *testing.T) {
	dir := t.TempDir()
	plant(t, dir, map[string]string{".env": "K=v\n", ".env.local": "K=v\n", ".env.ts": "export const k = 1;\n", "web/.env.production.js": "export {};\n",
		".envoy.ts": "export const envoy = 1;\n", "src/.environment.js": "export const e = 1;\n"})
	source, _, _, err := excerpts(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(maps.Keys(source)); !slices.Equal(got, []string{".envoy.ts", "src/.environment.js"}) {
		t.Fatalf("shown %q", got)
	}
}

// A NUL byte in a comment makes go-enry take a .ts file for binary and name no language: the file is still
// code the reviewer is shown, while a .ts that is a video's bytes is not, and is never named (#2280; the
// security review of PR 2303).
func TestANulByteNeverHidesCode(t *testing.T) {
	dir := t.TempDir()
	video := strings.Repeat("\x47\x00\xff\xfe", 5000) // an MPEG transport stream's sync byte, then binary
	plant(t, dir, map[string]string{"routes.ts": "// \x00\nexport function GET() { return new Response(\"ok\") }\n", "public/intro.ts": video,
		"long.ts": "// ab\n" + strings.Repeat("//\x00 é\n", 3000)}) // its 16 KiB end inside an é
	source, _, notUTF8, err := excerpts(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(maps.Keys(source)); !slices.Equal(got, []string{"long.ts", "routes.ts"}) || len(notUTF8) != 0 {
		t.Fatalf("shown %q, not UTF-8 %q", got, notUTF8)
	}
}

// Code under .git/, node_modules/ or vendor/ is never shown, and the run says where it sits, in one warning
// naming each folder once, never each file — and only where code sits, never a folder of stylesheets. It is
// no review that read nothing: the check keeps its outcome (contracts/finding.md; #2280).
func TestCodeInDependencyFoldersIsNamedOnce(t *testing.T) {
	dir := t.TempDir()
	plant(t, dir, map[string]string{"app.ts": "export const ok = 1;\n", "vendor/bundle/ruby/x/gem.rb": "module Gem\nend\n", "vendor/github.com/a/b.go": "package b\n",
		"web/node_modules/x/index.js": "module.exports = 1\n", "static/vendor/bootstrap.css": "a{}\n", "vendor/modules.txt": "x\n"})
	m := &model{}
	out, err := reviewer(t, m).Review(context.Background(), dir, finding.Log{})
	if err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(maps.Keys(m.calls[0].Files)); !slices.Equal(got, []string{"app.ts"}) {
		t.Fatalf("shown %q", got)
	}
	run := out.Runs[len(out.Runs)-1]
	said := slices.ContainsFunc(run.Invocations[0].ToolExecutionNotifications, func(n finding.Notification) bool {
		return n.Level == "warning" && n.Message.Text == `ai review · not checked: code in dependency folders: "vendor", "web/node_modules"`
	})
	if !said || Unread(run) != "" {
		t.Fatalf("one warning names the folders, and the review read the rest: %q, %+v", Unread(run), run.Invocations)
	}
	folders := map[string]string{}
	for i := range 21 {
		folders[fmt.Sprintf("p%02d/vendor/x.go", i)] = "package x\n"
	}
	plant(t, dir, folders)
	out, err = reviewer(t, &model{}).Review(context.Background(), dir, finding.Log{})
	if err != nil {
		t.Fatal(err)
	}
	notes := out.Runs[len(out.Runs)-1].Invocations[0].ToolExecutionNotifications
	if i := slices.IndexFunc(notes, func(n finding.Notification) bool {
		return strings.HasPrefix(n.Message.Text, "ai review · not checked: code in dependency folders: ")
	}); i < 0 ||
		!strings.HasPrefix(notes[i].Message.Text, `ai review · not checked: code in dependency folders: "p00/vendor", "p01/vendor", `) || !strings.HasSuffix(notes[i].Message.Text, `"p19/vendor", and 3 more`) {
		t.Fatalf("the first 20 folders, then the rest counted: %+v", notes)
	}
}

// More calls than maxCalls is over the ceiling: what a review sends is bounded, not only its source,
// since every call repeats the paths and each line its number (contracts/finding.md, #2274).
func TestMoreCallsThanTheCeilingAreTooLarge(t *testing.T) {
	dir, room := splitApp(t)
	more := map[string]string{}
	for i := range maxCalls * 2 {
		more[fmt.Sprintf("d/%02d.ts", i)] = fmt.Sprintf("export const g%d = 1;\n", i)
	}
	plant(t, dir, more)
	m := &model{}
	r := reviewer(t, m)
	all := append(slices.Collect(maps.Keys(more)), "a/1.ts", "a/2.ts", "b/3.ts", "b/4.ts", "c/5.ts")
	r.room = room - len(pathsBlock(all[len(more):])) + len(pathsBlock(all)) // the paths leave about two files' room: one call each two
	out, err := r.Review(context.Background(), dir, finding.Log{})
	if run := out.Runs[len(out.Runs)-1]; err != nil || len(m.calls) != 0 || Unread(run) != TooLarge {
		t.Fatalf("over %d calls the model is not called: %v, %d call(s), %+v", maxCalls, err, len(m.calls), run.Invocations)
	}
}

// An AI twin is sent with a part only while one of its files is in it: never with an empty list of files
// to check (contracts/finding.md).
func TestATwinIsSentOnlyWithItsFiles(t *testing.T) {
	dir, _ := splitApp(t)
	ruby, pad := "class UsersController < ApplicationController\nend\n", "// "+strings.Repeat("p", 97)+"\n"
	plant(t, dir, map[string]string{"r/users_controller.rb": ruby, "e/pad.ts": pad})
	all := []string{"a/1.ts", "a/2.ts", "b/3.ts", "b/4.ts", "c/5.ts", "e/pad.ts", "r/users_controller.rb"}
	m := &split{}
	r := reviewer(t, m)
	r.room = len(pathsBlock(all)) + len(fileBlock("r/users_controller.rb", ruby)) + 2*len(`"r/users_controller.rb", `) // the Ruby file and its two twins' lines fill a call
	if _, err := r.Review(context.Background(), dir, finding.Log{}); err != nil {
		t.Fatal(err)
	}
	twins := 0
	for _, c := range m.calls {
		_, ruby := c.Files["r/users_controller.rb"]
		_, rules := reviewPrompt(c)
		if has := slices.ContainsFunc(c.Rules, func(r finding.Rule) bool { return strings.HasPrefix(r.ID, "ai.") }); has != ruby || strings.Contains(rules, "Check only these files: \n") {
			t.Fatalf("a part with the Ruby file %v is sent the twins %v:\n%s", ruby, has, rules)
		}
		if ruby {
			twins++
		}
	}
	if len(m.calls) < 2 || twins != 1 {
		t.Fatalf("%d call(s), %d with the twins", len(m.calls), twins)
	}
}

// failing answers its part out of room or as a failure by the part's first file: whichever comes back
// first, a failure fails the review (contracts/finding.md).
type failing struct{}

func (failing) Review(_ context.Context, req Request) (Response, error) {
	if _, ok := req.Files["a/1.ts"]; ok {
		return Response{}, fmt.Errorf("%w: stopped at max_tokens", ErrOutOfRoom)
	}
	return Response{}, errors.New("500 Internal Server Error")
}

func TestAPartsFailureWinsOverAnotherOutOfRoom(t *testing.T) {
	dir, room := splitApp(t)
	for range 20 {
		r := reviewer(t, failing{})
		r.room = room
		if _, err := r.Review(context.Background(), dir, finding.Log{}); !errors.Is(err, ErrModel) {
			t.Fatalf("a failure and an out of room: %v", err)
		}
	}
}

// slow never answers before its call is stopped, then answers an error, or nothing when quiet.
type slow struct{ quiet bool }

func (m slow) Review(ctx context.Context, _ Request) (Response, error) {
	<-ctx.Done()
	if m.quiet {
		return Response{Results: []Candidate{}}, nil
	}
	return Response{}, fmt.Errorf("anthropic: %w", ctx.Err())
}

// A review whose calls do not answer in its time reads nothing, never a model failure: the run says so
// and goes on (contracts/finding.md).
func TestAReviewOutOfTimeIsNotCheckedAndTheRunGoesOn(t *testing.T) {
	dir, room := splitApp(t)
	r := reviewer(t, slow{})
	r.room, r.round = room, 50*time.Millisecond
	out, err := r.Review(context.Background(), dir, finding.Log{})
	if run := out.Runs[len(out.Runs)-1]; err != nil || len(run.Results) != 0 || Unread(run) != TimedOut || run.Invocations[0].ToolExecutionNotifications[0].Level != "warning" {
		t.Fatalf("out of time: %v, %+v", err, out.Runs)
	}
	r.Model = slow{quiet: true} // a model that answers after the time with no error is late all the same
	if out, err = r.Review(context.Background(), dir, finding.Log{}); err != nil || Unread(out.Runs[len(out.Runs)-1]) != TimedOut {
		t.Fatalf("a late answer with no error: %v, %+v", err, out.Runs)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Review(ctx, dir, finding.Log{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("the run's own end is its error, not the model's: %v", err)
	}
}

// A review every file of which is too large for a call calls no model, and says no second opinion.
func TestNoCallNoSecondOpinion(t *testing.T) {
	dir, room := splitApp(t)
	m := &model{}
	r := reviewer(t, m)
	r.room = len(pathsBlock([]string{"a/1.ts", "a/2.ts", "b/3.ts", "b/4.ts", "c/5.ts"})) + 10
	out, err := r.Review(context.Background(), dir, finding.Log{})
	run := out.Runs[len(out.Runs)-1]
	said := slices.ContainsFunc(run.Invocations[0].ToolExecutionNotifications, func(n finding.Notification) bool { return n.Message.Text == SecondOpinion })
	if err != nil || len(m.calls) != 0 || said || Unread(run) != TooLargeForACall || room == 0 {
		t.Fatalf("no call: %v, %d call(s), second opinion %v", err, len(m.calls), said)
	}
}

// The AI twins' scope lines are counted before a review is one call: files that fit as source but not
// with their twins' lines are split (contracts/finding.md).
func TestTwinScopesCountTowardsOneCall(t *testing.T) {
	order := []string{"a.rb", "b.rb"}
	text := map[string]string{"a.rb": "x\n", "b.rb": "y\n"}
	room := len(fileBlock("a.rb", "x\n")) + len(fileBlock("b.rb", "y\n"))
	if calls, paths, _, err := parts(order, text, text, nil, room); err != nil || len(calls) != 1 || paths != nil {
		t.Fatalf("without scopes the two fit one call: %d call(s), %v", len(calls), err)
	}
	scoped := scopeBytes(map[string][]string{"ai.sql-string-concat": order})
	if calls, _, _, _ := parts(order, text, text, scoped, room); len(calls) == 1 {
		t.Fatal("with their twins' lines they do not fit one call")
	}
}

// A verdict stored before the split still reads as one that read nothing, by its own note (#2274).
func TestAStoredOverTheOldCapNoteIsStillUnread(t *testing.T) {
	for agent, note := range map[string]string{"canyon-review": TooLargeBefore, CustomRules: Note(CustomRules, TooLargeBefore)} {
		run := finding.Run{Tool: finding.Tool{Driver: finding.Driver{Name: agent}}, Invocations: []finding.Invocation{{ToolExecutionNotifications: []finding.Notification{{Level: "warning", Message: finding.Message{Text: note}}}}}}
		if Unread(run) != TooLargeBefore {
			t.Fatalf("%s: %q", agent, Unread(run))
		}
	}
}

// A review that fits one call sends what it always did, so a recording made before the split still
// answers it; a split review's calls each key their own recording (#2274).
func TestARecordingStillAnswersAndASplitReviewReplays(t *testing.T) {
	m := &model{resp: Response{Results: []Candidate{}}}
	if _, err := reviewer(t, m).Review(context.Background(), claimsPortal, finding.Log{}); err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(m.calls[0]); bytes.Contains(raw, []byte(`"paths"`)) {
		t.Fatal("a review of one call names paths, which would move every recording's key")
	}
	dir, room := splitApp(t)
	live := &split{}
	r := reviewer(t, live)
	r.room = room
	want, err := r.Review(context.Background(), dir, finding.Log{})
	if err != nil {
		t.Fatal(err)
	}
	replay := Replay{}
	for _, req := range live.calls {
		resp, _ := (&split{}).Review(context.Background(), req)
		raw, _ := json.Marshal(req)
		replay[fmt.Sprintf("%x", sha256.Sum256(raw))] = resp
	}
	r.Model = replay
	got, err := r.Review(context.Background(), dir, finding.Log{})
	if err != nil || len(got.Runs[0].Results) != len(want.Runs[0].Results) || !got.Runs[0].Results[0].Properties.Canyon.Recorded {
		t.Fatalf("a split review replays part by part: %v", err)
	}
}

// A model whose answer ran out of room has not answered: the run goes on, the reviewer's run saying it
// checked nothing, never a model failure, its check never clean (contracts/finding.md; the test cluster's stock Next 16 app).
func TestAnAnswerOutOfRoomIsNotCheckedAndTheRunGoesOn(t *testing.T) {
	dir := t.TempDir()
	plant(t, dir, map[string]string{"app.ts": "export {};\n"})
	m := &model{err: fmt.Errorf("%w: anthropic stopped at max_tokens", ErrOutOfRoom)}
	for want, r := range map[string]*Reviewer{OutOfRoom: reviewer(t, m), "custom rules · not checked: the model ran out of room for its answer": custom(t, m, policy.Advisory)} {
		out, err := r.Review(context.Background(), dir, finding.Log{})
		if err != nil {
			t.Fatalf("%s: an answer out of room failed the review: %v", r.Agent, err)
		}
		if run := out.Runs[len(out.Runs)-1]; len(run.Results) != 0 || run.Invocations[0].ToolExecutionNotifications[0].Message.Text != want ||
			run.Invocations[0].ToolExecutionNotifications[0].Level != "warning" || Unread(run) != OutOfRoom {
			t.Fatalf("%s: the reviewer's run says it checked nothing: %+v", r.Agent, run)
		}
	}
}

// A candidate's columns are the real file's; on a masked literal's lines it stands, snippet masked.
func TestACandidateIsPlacedOnTheRealText(t *testing.T) {
	_, github, pem := credentials()
	dir := t.TempDir()
	lines := []string{"const gh = \"" + github + "\"; refetch();", "const key = `" + pem + "`;"}
	plant(t, dir, map[string]string{"app.ts": strings.Join(lines, "\n") + "\n"})
	c := candidate()
	c.File, c.StartLine, c.EndLine = "app.ts", 1, 1
	on := c
	on.StartLine, on.EndLine = 3, 3 // inside the PEM body
	out, err := reviewer(t, &model{resp: Response{Results: []Candidate{c, on}}}).Review(context.Background(), dir, finding.Log{})
	if err != nil {
		t.Fatal(err)
	}
	mustSchemas(t, out)
	got := out.Runs[0].Results[0].Locations[0].PhysicalLocation.Region
	if got.EndColumn != len([]rune(lines[0]))+1 {
		t.Fatalf("endColumn %d, want %d: columns count the real line", got.EndColumn, len([]rune(lines[0]))+1)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), github) || strings.Contains(string(raw), fake("pem-2", 60)) {
		t.Fatal("a credential reached the log through a candidate's snippet")
	}
}

// A candidate over the contract's ≤3-line snippet is windowed to its first three lines from
// start_line — trailing blank lines cut, never to a blank last line, at any length — and the review
// stands with every other candidate; it does not fail the run as the model's (canyonv2-1x5f).
func TestAnOverLongCandidateIsWindowedNotFailed(t *testing.T) {
	for name, tc := range map[string]struct{ start, end, wantEnd int }{
		"five lines":                     {61, 65, 63},
		"a blank line inside the window": {60, 65, 61}, // line 62 is blank
		"a blank last line, not over":    {61, 62, 61},
	} {
		t.Run(name, func(t *testing.T) {
			over := candidate()
			over.StartLine, over.EndLine = tc.start, tc.end
			m := &model{resp: Response{Results: []Candidate{over, elsewhere()}}}
			_, scanned := golden(t)
			out, err := reviewer(t, m).Review(context.Background(), claimsPortal, scanned)
			if err != nil {
				t.Fatalf("an over-long candidate failed the review: %v", err)
			}
			mustSchemas(t, out)
			if len(out.Runs) != 2 || len(out.Runs[0].Results) != len(scanned.Runs[0].Results) || len(out.Runs[1].Results) != 2 {
				t.Fatalf("the scanners' findings and both candidates stand: %d runs, %+v", len(out.Runs), out.Runs)
			}
			got := out.Runs[1].Results[0].Locations[0].PhysicalLocation.Region
			if got.StartLine != tc.start || got.EndLine != tc.wantEnd {
				t.Fatalf("region %d–%d, want %d–%d", got.StartLine, got.EndLine, tc.start, tc.wantEnd)
			}
			shown := strings.Split(m.calls[0].Files[over.File], "\n")[tc.start-1 : tc.wantEnd]
			if got.Snippet.Text != strings.Join(shown, "\n") {
				t.Fatalf("snippet %q, want the windowed lines as shown %q", got.Snippet.Text, shown)
			}
		})
	}
}

// A candidate whose title is over the profile's 80 characters is cut to fit — rune-aware, the last
// character an ellipsis — and the review stands with every other candidate and the scanners'
// findings; it does not fail the run as the model's (canyonv2-p97l).
func TestAnOverLongTitleIsCutNotFailed(t *testing.T) {
	long := candidate()
	long.Title = strings.Repeat("é", 120)
	m := &model{resp: Response{Results: []Candidate{long, elsewhere()}}}
	_, scanned := golden(t)
	out, err := reviewer(t, m).Review(context.Background(), claimsPortal, scanned)
	if err != nil {
		t.Fatalf("an over-long title failed the review: %v", err)
	}
	mustSchemas(t, out)
	if len(out.Runs) != 2 || len(out.Runs[0].Results) != len(scanned.Runs[0].Results) || len(out.Runs[1].Results) != 2 {
		t.Fatalf("the scanners' findings and both candidates stand: %d runs, %+v", len(out.Runs), out.Runs)
	}
	got := out.Runs[1].Results[0].Properties.Canyon.Title
	if utf8.RuneCountInString(got) != 80 || !strings.HasSuffix(got, "…") || !strings.HasPrefix(got, strings.Repeat("é", 79)) || out.Runs[1].Results[0].Message.Text != got {
		t.Fatalf("title %q: want the first 79 characters and an ellipsis", got)
	}
	if other := out.Runs[1].Results[1].Properties.Canyon.Title; other != candidate().Title {
		t.Fatalf("a title within the cap is kept: %q", other)
	}
	words := candidate() // the QA batch's: cut at a space, never mid-word
	words.Title = "No auth app or authentication middleware configured, so no route can require sign-in at all"
	if out, err = reviewer(t, &model{resp: Response{Results: []Candidate{words}}}).Review(context.Background(), claimsPortal, scanned); err != nil {
		t.Fatal(err)
	}
	if got := out.Runs[1].Results[0].Properties.Canyon.Title; got != "No auth app or authentication middleware configured, so no route can require…" {
		t.Fatalf("title %q: want it cut at the last space", got)
	}
}

// A candidate that starts on a blank line starts on the next non-blank line of its span instead,
// windowed from there; the review stands (canyonv2-p97l). Line 62 and 64 of the fixture are blank.
func TestABlankStartLineMovesToTheNextNonBlank(t *testing.T) {
	for name, tc := range map[string]struct{ start, end, wantStart, wantEnd int }{
		"one blank line then code":      {62, 63, 63, 63},
		"windowed from the moved start": {62, 65, 63, 65},
	} {
		t.Run(name, func(t *testing.T) {
			blank := candidate()
			blank.StartLine, blank.EndLine = tc.start, tc.end
			m := &model{resp: Response{Results: []Candidate{blank, candidate()}}}
			_, scanned := golden(t)
			out, err := reviewer(t, m).Review(context.Background(), claimsPortal, scanned)
			if err != nil {
				t.Fatalf("a blank-start candidate failed the review: %v", err)
			}
			mustSchemas(t, out)
			if len(out.Runs) != 2 || len(out.Runs[0].Results) != len(scanned.Runs[0].Results) || len(out.Runs[1].Results) != 2 {
				t.Fatalf("the scanners' findings and both candidates stand: %d runs, %+v", len(out.Runs), out.Runs)
			}
			got := out.Runs[1].Results[0].Locations[0].PhysicalLocation.Region
			if got.StartLine != tc.wantStart || got.EndLine != tc.wantEnd {
				t.Fatalf("region %d–%d, want %d–%d", got.StartLine, got.EndLine, tc.wantStart, tc.wantEnd)
			}
			shown := strings.Split(m.calls[0].Files[blank.File], "\n")[tc.wantStart-1 : tc.wantEnd]
			if got.Snippet.Text != strings.Join(shown, "\n") {
				t.Fatalf("snippet %q, want the lines as shown %q", got.Snippet.Text, shown)
			}
		})
	}
}

// The answer's schema tells the model each cap the profile holds it to, so it rarely exceeds one.
func TestTheCandidateSchemaStatesTheCaps(t *testing.T) {
	raw, err := json.Marshal(answerSchema(nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, cap := range []string{"at most 3 lines", "at most 80 characters"} {
		if !strings.Contains(string(raw), cap) {
			t.Fatalf("the candidate schema does not state %q: %s", cap, raw)
		}
	}
}

// Recorded is Replay's to set: a recording or a model's JSON cannot claim it.
func TestRecordedIsNeverReadOffTheWire(t *testing.T) {
	var resp Response
	if err := json.Unmarshal([]byte(`{"Recorded": true, "recorded": true, "results": []}`), &resp); err != nil || resp.Recorded {
		t.Fatalf("Recorded came off the wire: %v %+v", err, resp)
	}
}

func candidate() Candidate {
	return Candidate{Rule: "perf.unbounded-refetch", File: "apps/web/src/components/ClaimsTable.tsx", StartLine: 61, EndLine: 61,
		Severity: "medium", Title: "ClaimsTable refetches on each keystroke", Problem: "A full scan per character.", Fix: "Debounce it."}
}

// elsewhere is candidate on another line of the file: another finding, which a repeat of candidate's
// region would not be.
func elsewhere() Candidate {
	c := candidate()
	c.StartLine, c.EndLine = 63, 63
	return c
}

// A model that fails fails the run as the model's, reason rendered.
func TestAModelFailureFailsTheRunClosed(t *testing.T) {
	for name, m := range map[string]*model{
		"429":     {err: errors.New("429 Too Many Requests")},
		"500":     {err: errors.New("500 Internal Server Error")},
		"timeout": {err: context.DeadlineExceeded},
	} {
		t.Run(name, func(t *testing.T) {
			_, scanned := golden(t)
			out, err := reviewer(t, m).Review(context.Background(), claimsPortal, scanned)
			if !errors.Is(err, ErrModel) || len(out.Runs) != 0 || !strings.Contains(err.Error(), m.err.Error()) {
				t.Fatalf("the run fails as the model's failure, reason rendered: %v, %d runs", err, len(out.Runs))
			}
		})
	}
}

// A candidate the profile refuses costs that candidate alone (canyonv2-4lr6): it is dropped and
// counted as one outside the reviewer's rules is, nothing of it reaches the log, and the rest of the
// review stands — an answer of nothing else too, with no results and the count.
func TestACandidateTheProfileRefusesIsDroppedAndSurfaced(t *testing.T) {
	_, gh, _ := credentials()
	review := func(t *testing.T, results []Candidate, want string) finding.Run {
		t.Helper()
		r := reviewer(t, &model{resp: Response{Results: results}})
		_, scanned := golden(t)
		out, err := r.Review(context.Background(), claimsPortal, scanned)
		if err != nil {
			t.Fatalf("the review stands: %v", err)
		}
		if raw := mustSchemas(t, out); strings.Contains(string(raw), gh) {
			t.Fatal("a dropped candidate's credential reached the log")
		}
		if err := cmp.Or(finding.Derive(&out, r.Snapshot), finding.Check(&out, r.Snapshot)); err != nil {
			t.Fatalf("what is left passes seam 1 against the snapshot: %v", err)
		}
		run := out.Runs[1]
		if inv := run.Invocations; len(inv) != 1 || !slices.Equal(inv[0].ToolExecutionNotifications,
			[]finding.Notification{{Level: "note", Message: finding.Message{Text: want}}, secondOpinion}) {
			t.Fatalf("the drop is surfaced once, as a count: %+v", inv)
		}
		return run
	}
	var all []Candidate
	for name, edit := range map[string]func(*Candidate){
		"a file not shown":    func(c *Candidate) { c.File = "apps/web/src/nope.ts" },
		"a line past the end": func(c *Candidate) { c.StartLine, c.EndLine = 900, 900 },
		"a blank line":        func(c *Candidate) { c.StartLine, c.EndLine = 62, 62 },
		"a credential echoed": func(c *Candidate) { c.Fix = "Rotate " + gh + "." },
	} {
		c := candidate()
		edit(&c)
		all = append(all, c)
		t.Run(name, func(t *testing.T) {
			run := review(t, []Candidate{c, candidate()}, "ai review · dropped 1 invalid result(s)")
			if len(run.Results) != 1 || run.Results[0].Message.Text != candidate().Title {
				t.Fatalf("the good candidate stands and no other does: %+v", run.Results)
			}
		})
	}
	t.Run("nothing else", func(t *testing.T) {
		if run := review(t, all, "ai review · dropped 4 invalid result(s)"); len(run.Results) != 0 {
			t.Fatalf("no candidate stands: %+v", run.Results)
		}
	})
}

// A candidate with its rule, file and lines but an empty title, problem or fix is kept, the empty field
// in Canyon's words for its rule, and nothing is dropped or counted (#1962: a review dropped the one
// unauthenticated route it found for an empty field and said "findings: 0").
func TestACandidateWithAnEmptyFieldIsKeptInCanyonsWords(t *testing.T) {
	said := candidate()
	said.Rule = "auth.unauthenticated-route"
	problem := `Canyon's AI review found that this code breaks "Every route needs sign-in" (auth.unauthenticated-route) and gave no more detail.`
	fix := `Change this code so it follows "Every route needs sign-in": Canyon's review then no longer reports auth.unauthenticated-route here.`
	for name, c := range map[string]struct {
		edit                func(*Candidate)
		title, problem, fix string
	}{
		"no title":    {func(c *Candidate) { c.Title = "" }, "Every route needs sign-in", said.Problem, said.Fix},
		"no problem":  {func(c *Candidate) { c.Problem = "" }, said.Title, problem, said.Fix},
		"no fix":      {func(c *Candidate) { c.Fix = "" }, said.Title, said.Problem, fix},
		"a blank fix": {func(c *Candidate) { c.Fix = " \n " }, said.Title, said.Problem, fix},
	} {
		t.Run(name, func(t *testing.T) {
			cand := said
			c.edit(&cand)
			r := reviewer(t, &model{resp: Response{Results: []Candidate{cand}}})
			_, scanned := golden(t)
			out, err := r.Review(context.Background(), claimsPortal, scanned)
			if err != nil {
				t.Fatal(err)
			}
			mustSchemas(t, out)
			if err := cmp.Or(finding.Derive(&out, r.Snapshot), finding.Check(&out, r.Snapshot)); err != nil {
				t.Fatalf("the result passes seam 1 against the snapshot: %v", err)
			}
			run := out.Runs[1]
			if len(run.Results) != 1 {
				t.Fatalf("the candidate is kept: %+v", run.Invocations)
			}
			if got := run.Results[0].Properties.Canyon; got.Title != c.title || got.Problem != c.problem || got.Fix != c.fix {
				t.Fatalf("the empty field reads Canyon's words: %+v", got)
			}
			if notes := run.Invocations[0].ToolExecutionNotifications; !slices.Equal(notes, []finding.Notification{secondOpinion}) {
				t.Fatalf("nothing is dropped: %+v", notes)
			}
		})
	}
}

// A model that says one finding twice — the same rule, file and lines, in other words — keeps one
// result: two would ask for the fix, and an approval, twice (the tester run's GET /api/results). The one
// of highest severity stands, whatever order the model gave them in, and the rest are dropped and
// counted; the same words on other lines are another finding.
func TestARepeatedCandidateIsSaidOnce(t *testing.T) {
	again, higher, elsewhere, equal := candidate(), candidate(), candidate(), candidate()
	again.Title, again.Problem = "ClaimsTable refetches every claim on each keystroke", "Each character scans the table."
	equal.Title = "ClaimsTable refetches on every keystroke, said again"
	higher.Severity, higher.Title = "high", "ClaimsTable runs a full scan per keystroke"
	elsewhere.StartLine, elsewhere.EndLine = 63, 63
	r := reviewer(t, &model{resp: Response{Results: []Candidate{candidate(), again, higher, elsewhere, equal}}})
	_, scanned := golden(t)
	out, err := r.Review(context.Background(), claimsPortal, scanned)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmp.Or(finding.Derive(&out, r.Snapshot), finding.Check(&out, r.Snapshot)); err != nil {
		t.Fatalf("what is left passes seam 1 against the snapshot: %v", err)
	}
	run := out.Runs[1]
	if len(run.Results) != 2 || run.Results[0].Message.Text != higher.Title || run.Results[1].Locations[0].PhysicalLocation.Region.StartLine != 63 {
		t.Fatalf("one result per rule and region, the highest severity kept: %+v", run.Results)
	}
	if inv := run.Invocations; len(inv) != 1 || !slices.Equal(inv[0].ToolExecutionNotifications,
		[]finding.Notification{{Level: "note", Message: finding.Message{Text: "ai review · dropped 3 invalid result(s)"}}, secondOpinion}) {
		t.Fatalf("the repeats are dropped and counted: %+v", inv)
	}
	firsts := reviewer(t, &model{resp: Response{Results: []Candidate{candidate(), again}}})
	out, err = firsts.Review(context.Background(), claimsPortal, scanned)
	if err != nil || len(out.Runs[1].Results) != 1 || out.Runs[1].Results[0].Message.Text != candidate().Title {
		t.Fatalf("among equals the first in the answer stands: %v %+v", err, out.Runs[1].Results)
	}
}

// Each dropped candidate is one server log line naming the run, the project, the reason's fixed key and
// the rule — never the candidate's text, nor a rule the model made up, which is its text too — so a
// model echoing a credential, or Canyon refusing a real finding, is seen once, not only counted.
func TestADroppedCandidateIsLoggedByReasonAndRuleAndNothingElse(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	_, gh, _ := credentials()
	echoed, madeUp, blank := candidate(), candidate(), candidate()
	echoed.Fix = "Rotate " + gh + "."
	madeUp.Rule = "perf." + gh
	blank.StartLine, blank.EndLine = 62, 62
	r := reviewer(t, &model{resp: Response{Results: []Candidate{echoed, madeUp, blank, candidate()}}})
	_, scanned := golden(t)
	if _, err := r.Review(WithRun(context.Background(), "r_7f3a9c", "claims-portal"), claimsPortal, scanned); err != nil {
		t.Fatal(err)
	}
	logged := buf.String()
	for _, want := range []string{
		"run r_7f3a9c (claims-portal): ai review dropped a candidate: credential, rule perf.unbounded-refetch\n",
		"run r_7f3a9c (claims-portal): ai review dropped a candidate: unknown_rule\n",
		"run r_7f3a9c (claims-portal): ai review dropped a candidate: blank, rule perf.unbounded-refetch\n",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("the log has no line %q:\n%s", want, logged)
		}
	}
	if strings.Count(logged, "\n") != 3 || strings.Contains(logged, gh) || strings.Contains(logged, "Rotate") {
		t.Fatalf("one line per drop and nothing of a candidate's text:\n%s", logged)
	}
}

// A candidate under a rule the reviewer may not emit — outside its namespace, Off, or no rule at
// all — is dropped and counted in one notification with the candidates the profile refuses; the
// rest of the review stands (canyonv2-95s).
func TestACandidateOutsideItsRulesIsDroppedAndSurfaced(t *testing.T) {
	outside := func(rule string) Candidate {
		c := candidate()
		c.Rule = rule
		return c
	}
	unshown := candidate()
	unshown.File = "apps/web/src/nope.ts"
	m := &model{resp: Response{Results: []Candidate{outside("secrets.client-bundle"), candidate(), outside("perf.switched-off"), outside("perf.no-such-rule"), unshown}}}
	r := reviewer(t, m)
	r.Snapshot.Rules = append(r.Snapshot.Rules, policy.Rule{ID: "perf.switched-off", Name: "A perf rule an admin turned off", State: policy.Off, Producer: policy.LLMReview})
	_, scanned := golden(t)
	out, err := r.Review(context.Background(), claimsPortal, scanned)
	if err != nil {
		t.Fatal(err)
	}
	mustSchemas(t, out)
	if err := cmp.Or(finding.Derive(&out, r.Snapshot), finding.Check(&out, r.Snapshot)); err != nil {
		t.Fatalf("what is left passes seam 1 against the snapshot: %v", err)
	}
	run := out.Runs[1]
	if len(run.Results) != 1 || run.Results[0].RuleID != "perf.unbounded-refetch" {
		t.Fatalf("the candidate under its own rule stands and no other does: %+v", run.Results)
	}
	if inv := run.Invocations; len(inv) != 1 || !slices.Equal(inv[0].ToolExecutionNotifications,
		[]finding.Notification{{Level: "note", Message: finding.Message{Text: "ai review · dropped 4 invalid result(s)"}}, secondOpinion}) {
		t.Fatalf("the drop is surfaced once, as a count: %+v", inv)
	}
}

// The scanners' own rules stay strict: a scanner result outside the scanners' namespace is a bug,
// and the log fails closed rather than dropping it.
func TestAScannerResultOutsideItsNamespaceFailsClosed(t *testing.T) {
	reviewed, scanned := golden(t)
	snap := reviewer(t, nil).Snapshot
	perf := reviewed.Runs[1].Results[len(reviewed.Runs[1].Results)-1] // the reviewer's perf.unbounded-refetch, under the scanners' driver
	scanned.Runs[0].Results = append(scanned.Runs[0].Results, perf)
	scanned.Runs[0].Tool.Driver.Rules = append(scanned.Runs[0].Tool.Driver.Rules, finding.Rule{ID: "perf.unbounded-refetch", Name: "No unbounded refetch on user input"})
	if err := finding.Check(&scanned, snap); err == nil || !strings.Contains(err.Error(), "outside the namespace") {
		t.Fatalf("an out-of-namespace scanner result did not fail closed: %v", err)
	}
}

// A severity the model wrote that is not one of the four is read as the highest, never dropped.
func TestAnUnparseableSeverityIsTheHighest(t *testing.T) {
	c := candidate()
	c.Severity = "Medium-ish"
	_, scanned := golden(t)
	out, err := reviewer(t, &model{resp: Response{Results: []Candidate{c}}}).Review(context.Background(), claimsPortal, scanned)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.Runs[1].Results[0].Properties.Canyon.Severity; got != finding.Critical {
		t.Fatalf("severity %q, want critical", got)
	}
}

// An admin may set a reviewer's rule to blocking (canyonv2-iic1): a candidate that will block carries
// its governance prose into the finding — do_not fitted to one line an entry, an empty one left out —
// and one that will block without a required_end_state or a do_not is not dropped: Canyon supplies
// both, in finding.md's exact words, so the finding stands and the ship stops (canyonv2-ihqr). Every
// one passes seam 1, and at a severity that does not block, none is needed.
func TestABlockingAICandidateWithoutGovernanceGetsCanyons(t *testing.T) {
	governed, bare, noDoNot, noEndState, advisory := candidate(), candidate(), candidate(), candidate(), candidate()
	governed.Severity, bare.Severity, noDoNot.Severity, noEndState.Severity = "high", "high", "critical", "high"
	governed.RequiredEndState, governed.DoNot = "The table fetches once per settled query.", []string{"raise the page size\n instead", "  "}
	noDoNot.RequiredEndState, noDoNot.DoNot = "The table fetches once.", []string{" "}
	noEndState.DoNot = []string{"raise the page size"}
	for i, c := range []*Candidate{&bare, &noDoNot, &noEndState, &advisory} { // five findings, each on its own line: one region twice is one finding
		c.StartLine, c.EndLine = 63+i, 63+i
	}
	r := reviewer(t, &model{resp: Response{Results: []Candidate{governed, bare, noDoNot, noEndState, advisory}}})
	for i := range r.Snapshot.Rules {
		if r.Snapshot.Rules[i].ID == governed.Rule {
			r.Snapshot.Rules[i].State = policy.Blocking
		}
	}
	_, scanned := golden(t)
	out, err := r.Review(context.Background(), claimsPortal, scanned)
	if err != nil {
		t.Fatalf("the review stands: %v", err)
	}
	if err := cmp.Or(finding.Derive(&out, r.Snapshot), finding.Check(&out, r.Snapshot)); err != nil {
		t.Fatalf("a blocking AI finding passes seam 1: %v", err)
	}
	run := out.Runs[1]
	if len(run.Invocations) != 1 || !slices.Equal(run.Invocations[0].ToolExecutionNotifications, []finding.Notification{secondOpinion}) || len(run.Results) != 5 {
		t.Fatalf("no candidate is dropped: %+v %+v", run.Invocations, run.Results)
	}
	c := run.Results[0].Properties.Canyon
	if c.Blocks == nil || !*c.Blocks || c.RequiredEndState != governed.RequiredEndState || !slices.Equal(c.DoNot, []string{"raise the page size instead"}) {
		t.Fatalf("the blocking finding carries its governance prose: %+v", c)
	}
	canyons := []string{"Silence it with an ignore comment or a suppression pragma — Canyon does not read those.",
		"Remove or rename the flagged code just so the review stops matching it."}
	for _, res := range run.Results[1:4] {
		c := res.Properties.Canyon
		want := "Canyon's review no longer reports " + res.RuleID + " for this code when it runs again, and the app still does what it did."
		if c.Blocks == nil || !*c.Blocks || c.RequiredEndState != want || !slices.Equal(c.DoNot, canyons) {
			t.Fatalf("a blocking finding without governance stands with Canyon's: %+v", c)
		}
	}
	if adv := run.Results[4].Properties.Canyon; adv.Blocks == nil || *adv.Blocks || adv.RequiredEndState != "" {
		t.Fatalf("the advisory finding neither blocks nor needs prose: %+v", run.Results[4].Properties.Canyon)
	}
}

// Under a rule the admin left advisory, a high candidate without governance stands as it is: it will
// not block, so Canyon adds no prose.
func TestAnAdvisoryRuleNeedsNoGovernance(t *testing.T) {
	bare := candidate()
	bare.Severity = "high"
	r := reviewer(t, &model{resp: Response{Results: []Candidate{bare}}})
	_, scanned := golden(t)
	out, err := r.Review(context.Background(), claimsPortal, scanned)
	if err != nil {
		t.Fatal(err)
	}
	if err := finding.Derive(&out, r.Snapshot); err != nil {
		t.Fatal(err)
	}
	if c := out.Runs[1].Results[0].Properties.Canyon; c.Blocks == nil || *c.Blocks || c.RequiredEndState != "" || len(c.DoNot) != 0 {
		t.Fatalf("an advisory finding carries no Canyon prose: %+v", c)
	}
}

// A rule Off before preview and placed Blocking before production is on: the model is asked about
// it and its candidate stands, and one that will block before production without governance carries
// Canyon's, while its blocks stays before preview's — false (contracts/snapshot.md, Two gates;
// finding.md).
func TestARulePlacedBeforeProductionAloneIsReviewed(t *testing.T) {
	bare := candidate()
	bare.Severity = "high"
	m := &model{resp: Response{Results: []Candidate{bare}}}
	r := reviewer(t, m)
	for i := range r.Snapshot.Rules {
		if r.Snapshot.Rules[i].ID == bare.Rule {
			r.Snapshot.Rules[i].State, r.Snapshot.Rules[i].Production = policy.Off, policy.Blocking
		}
	}
	_, scanned := golden(t)
	out, err := r.Review(context.Background(), claimsPortal, scanned)
	if err == nil {
		err = finding.Derive(&out, r.Snapshot)
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(m.calls) != 1 || !slices.ContainsFunc(m.calls[0].Rules, func(x finding.Rule) bool { return x.ID == bare.Rule }) {
		t.Fatalf("the model is asked about the placed rule: %+v", m.calls)
	}
	c := out.Runs[1].Results[0].Properties.Canyon
	want := "Canyon's review no longer reports " + bare.Rule + " for this code when it runs again, and the app still does what it did."
	if len(out.Runs[1].Results) != 1 || c.Blocks == nil || *c.Blocks || c.RequiredEndState != want || len(c.DoNot) != 2 {
		t.Fatalf("the candidate stands with Canyon's prose and blocks nothing before preview: %+v", out.Runs[1])
	}
}

// The reviewer is shown every file of an application language by Coverage's reading of it, so a Rails
// app's Ruby without .rb — config.ru, Rakefile, Gemfile, a bin/ script — and a C file are shown beside
// its templates, and nothing vendored, under .git/ or node_modules/, or named .env* (contracts/finding.md,
// The reviewer's run; #1667: by extension it was shown 24 of a stock Rails app's 41 Ruby files).
func TestTheReviewerIsShownEveryFileOfAnApplicationLanguage(t *testing.T) {
	dir := t.TempDir()
	tree := map[string]string{
		"config.ru":                     "require_relative \"config/environment\"\nrun Rails.application\n",
		"Rakefile":                      "require_relative \"config/application\"\nRails.application.load_tasks\n",
		"Gemfile":                       "source \"https://rubygems.org\"\ngem \"rails\"\n",
		"bin/rails":                     "#!/usr/bin/env ruby\nAPP_PATH = File.expand_path(\"../config/application\", __dir__)\nrequire \"rails/commands\"\n",
		"lib/tasks/seed.rake":           "task :seed do\nend\n",
		"app/models/user.rb":            "class User < ApplicationRecord\nend\n",
		"app/api/external/route.ts":     "export function GET() { return new Response(\"ok\") }\n", // Linguist calls external/ vendored; it is the app\'s
		"app/views/users/show.html.erb": "<p><%= @user.name %></p>\n",
		"native/hash.c":                 "int main(void) { return 0; }\n",
		"README.md":                     "hello\n",
		"bin/docker-entrypoint":         "#!/bin/bash -e\nexec \"$@\"\n",
		"vendor/bundle/ruby/x/gem.rb":   "module Gem\nend\n",
		"node_modules/x/index.js":       "module.exports = 1\n",
		".env.rb":                       "SECRET = 1\n",
	}
	plant(t, dir, tree)
	source, _, _, err := excerpts(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := slices.Sorted(maps.Keys(source))
	want := []string{"Gemfile", "Rakefile", "app/api/external/route.ts", "app/models/user.rb", "app/views/users/show.html.erb", "bin/rails", "config.ru", "lib/tasks/seed.rake", "native/hash.c"}
	if !slices.Equal(got, want) {
		t.Fatalf("shown:\n%q\nwant:\n%q", got, want)
	}
}

// A Rails controller no scanner rule reads for SQL or code injection: the AI review is sent each twin
// for it alone, scoped to that file in Canyon's words, never as an admin's; a candidate on it is one advisory
// finding, and a candidate for a twin on a file a scanner reads is dropped (contracts/finding.md, The
// AI review where no scanner reads the language).
func TestTheAIReviewChecksWhatNoScannerReads(t *testing.T) {
	dir := t.TempDir()
	plant(t, dir, map[string]string{
		"app/controllers/users_controller.rb": "class UsersController < ApplicationController\n  def index\n    @users = User.where(\"name = '#{params[:name]}'\")\n  end\nend\n",
		"app.py":                              "def search(db, name):\n    return db.execute(\"select * from users where name = '\" + name + \"'\")\n",
		"lib/legacy.rb":                       "# caf\xe9\nclass Legacy\nend\n", // Latin-1: the reviewer is never shown it, so never scoped to it
		"config/.env.rb":                      "X = 1\n",                        // nor a .env* file
	})
	m := &model{}
	for _, ai := range []string{"ai.sql-string-concat", "ai.code-injection"} {
		for _, at := range []struct {
			file string
			line int
		}{{"app/controllers/users_controller.rb", 3}, {"app.py", 2}} {
			m.resp.Results = append(m.resp.Results, Candidate{Rule: ai, File: at.file, StartLine: at.line, EndLine: at.line, Severity: "high",
				Title: "Hand-built SQL from a request parameter", Problem: "The name is spliced into the query.", Fix: "Bind it as a parameter."})
		}
	}
	m.resp.Results = m.resp.Results[:3] // ai.code-injection is answered on the controller alone
	r := reviewer(t, m)
	out, err := r.Review(context.Background(), dir, finding.Log{})
	if err != nil {
		t.Fatal(err)
	}
	if err := finding.Derive(&out, r.Snapshot); err != nil {
		t.Fatal(err)
	}
	req := m.calls[0]
	controller := []string{"app/controllers/users_controller.rb"}
	if want := map[string][]string{"ai.sql-string-concat": controller, "ai.code-injection": controller}; req.Instructions != nil || !maps.EqualFunc(req.Scope, want, slices.Equal) {
		t.Fatalf("instructions %q, scope %q", req.Instructions, req.Scope)
	}
	if _, rules := reviewPrompt(req); !strings.Contains(rules, "- ai.sql-string-concat: No hand-built SQL from user input (AI review)\n  Check only these files: \"app/controllers/users_controller.rb\"\n") {
		t.Fatalf("the prompt's rules:\n%s", rules)
	}
	run := out.Runs[len(out.Runs)-1]
	var got []string
	for _, res := range run.Results {
		got = append(got, fmt.Sprintf("%s %s:%d blocks=%v", res.RuleID, res.Locations[0].PhysicalLocation.ArtifactLocation.URI, res.Locations[0].PhysicalLocation.Region.StartLine, *res.Properties.Canyon.Blocks))
	}
	if !slices.Equal(got, []string{"ai.sql-string-concat app/controllers/users_controller.rb:3 blocks=false", "ai.code-injection app/controllers/users_controller.rb:3 blocks=false"}) {
		t.Fatalf("results: %q", got)
	}
	if notes := run.Invocations[0].ToolExecutionNotifications; notes[len(notes)-2].Message.Text != "ai review · dropped 1 invalid result(s)" {
		t.Fatalf("the candidate outside its scope is dropped: %+v", notes)
	}
}

// With no rule to emit — every rule of the agent off — the model is not called: an answer could name
// no rule the schema allows. The run has no results and says nothing.
func TestWithNoRuleToEmitTheModelIsNotCalled(t *testing.T) {
	m := &model{}
	r := reviewer(t, m)
	snap, err := policy.WithStates(r.Snapshot, map[string]policy.State{"auth.unauthenticated-route": policy.Off, "perf.unbounded-refetch": policy.Off})
	if err != nil {
		t.Fatal(err)
	}
	r.Snapshot = snap
	_, scanned := golden(t)
	out, err := r.Review(context.Background(), claimsPortal, scanned)
	if err != nil {
		t.Fatal(err)
	}
	mustSchemas(t, out)
	if run := out.Runs[len(out.Runs)-1]; len(m.calls) != 0 || len(run.Results) != 0 || len(run.Invocations) != 0 || len(run.Tool.Driver.Rules) != 0 {
		t.Fatalf("%d model calls, run %+v", len(m.calls), run)
	}
}

// With every rule but the AI twins off, a twin whose only files are not valid UTF-8 is sent no call, and the
// run still names them and reads as one that read nothing — never a clean run over code nobody read. A file
// of a language a scanner reads is no twin's, and is not named (#2280; codex on PR 2303).
func TestATwinsUnreadableFilesAreNamedWithNoCall(t *testing.T) {
	dir := t.TempDir()
	plant(t, dir, map[string]string{"app/legacy.rb": "# caf\xe9\nclass Legacy\nend\n", "app.py": "# caf\xe9\nx = 1\n"})
	m := &model{}
	r := reviewer(t, m)
	snap, err := policy.WithStates(r.Snapshot, map[string]policy.State{"auth.unauthenticated-route": policy.Off, "perf.unbounded-refetch": policy.Off})
	if err != nil {
		t.Fatal(err)
	}
	r.Snapshot = snap
	out, err := r.Review(context.Background(), dir, finding.Log{})
	if err != nil {
		t.Fatal(err)
	}
	run := out.Runs[len(out.Runs)-1]
	if len(m.calls) != 0 || len(run.Invocations) != 1 || len(run.Invocations[0].ToolExecutionNotifications) != 1 ||
		run.Invocations[0].ToolExecutionNotifications[0].Message.Text != `ai review · not checked: not valid UTF-8: "app/legacy.rb"` || Unread(run) != NotUTF8 {
		t.Fatalf("%d model calls, run %+v", len(m.calls), run.Invocations)
	}
}

// With every rule but the AI twins off, a code file go-enry names no language for (a NUL byte in a comment)
// is one no scanner rule is known to read, so the twins check it; and with nothing to send, the folders of
// dependency code are still named (#2280; codex round 2 on PR 2303).
func TestTwinsTakeCodeOfNoLanguageAndNameDependencyFolders(t *testing.T) {
	twinsOnly := func(t *testing.T, m Model) *Reviewer {
		r := reviewer(t, m)
		snap, err := policy.WithStates(r.Snapshot, map[string]policy.State{"auth.unauthenticated-route": policy.Off, "perf.unbounded-refetch": policy.Off})
		if err != nil {
			t.Fatal(err)
		}
		r.Snapshot = snap
		return r
	}
	dir := t.TempDir()
	plant(t, dir, map[string]string{"native/hash.h": "/* \x00 */\nint hash(const char *s);\n"})
	m := &model{}
	if _, err := twinsOnly(t, m).Review(context.Background(), dir, finding.Log{}); err != nil {
		t.Fatal(err)
	}
	if len(m.calls) != 1 || !slices.Equal(m.calls[0].Scope["ai.sql-string-concat"], []string{"native/hash.h"}) {
		t.Fatalf("calls %+v", m.calls)
	}
	dir = t.TempDir()
	plant(t, dir, map[string]string{"vendor/bundle/ruby/x/gem.rb": "module Gem\nend\n", "app.py": "x = 1\n"})
	m = &model{}
	out, err := twinsOnly(t, m).Review(context.Background(), dir, finding.Log{})
	if err != nil {
		t.Fatal(err)
	}
	run := out.Runs[len(out.Runs)-1]
	if len(m.calls) != 0 || len(run.Invocations) != 1 || run.Invocations[0].ToolExecutionNotifications[0].Message.Text != `ai review · not checked: code in dependency folders: "vendor"` {
		t.Fatalf("%d calls, run %+v", len(m.calls), run.Invocations)
	}
}

// secondOpinion is the note every run the model answered carries last.
var secondOpinion = finding.Notification{Level: "note", Message: finding.Message{Text: "ai review · a model's second opinion: another review, even of a small change, may find different things"}}

// A run the model answered says the review is a model's second opinion, last of its notes, results or
// none, replayed or not, the custom-rules agent's in its own words; a run the model did not answer says
// nothing of it (#1819: the same rules, a one-line fix, and three findings the first review had not).
func TestAnAnsweredReviewSaysItIsASecondOpinion(t *testing.T) {
	last := func(run finding.Run) finding.Notification {
		if len(run.Invocations) != 1 || len(run.Invocations[0].ToolExecutionNotifications) == 0 {
			return finding.Notification{}
		}
		notes := run.Invocations[0].ToolExecutionNotifications
		return notes[len(notes)-1]
	}
	want := secondOpinion
	for name, resp := range map[string]Response{
		"results":  {Results: []Candidate{candidate()}},
		"none":     {Results: []Candidate{}},
		"replayed": {Recorded: true},
		"dropped":  {Results: []Candidate{{Rule: "sql.string-concat", File: "x"}}},
	} {
		t.Run(name, func(t *testing.T) {
			_, scanned := golden(t)
			out, err := reviewer(t, &model{resp: resp}).Review(context.Background(), claimsPortal, scanned)
			if err != nil {
				t.Fatal(err)
			}
			mustSchemas(t, out)
			if got := last(out.Runs[len(out.Runs)-1]); got != want {
				t.Fatalf("the answered review says it is a second opinion, last: %+v", got)
			}
		})
	}
	t.Run("custom rules", func(t *testing.T) {
		r := custom(t, &model{resp: Response{}}, policy.Advisory)
		_, scanned := golden(t)
		out, err := r.Review(context.Background(), claimsPortal, scanned)
		if err != nil {
			t.Fatal(err)
		}
		if got := last(out.Runs[len(out.Runs)-1]).Message.Text; got != "custom rules · a model's second opinion: another review, even of a small change, may find different things" {
			t.Fatalf("the custom-rules run says it in its own words: %q", got)
		}
	})
	for name, m := range map[string]Model{"no model": nil, "out of room": &model{err: ErrOutOfRoom}} {
		t.Run(name, func(t *testing.T) {
			_, scanned := golden(t)
			out, err := reviewer(t, m).Review(context.Background(), claimsPortal, scanned)
			if err != nil {
				t.Fatal(err)
			}
			if notes := out.Runs[len(out.Runs)-1].Invocations[0].ToolExecutionNotifications; len(notes) != 1 || notes[0] == want {
				t.Fatalf("a review the model did not answer says only why: %+v", notes)
			}
		})
	}
}

// On a host that signs people in, an auth.unauthenticated-route result says so in Canyon's words: the
// route is reached only by people Canyon signed in, and who is calling is the identity token's, never a
// login of the app's own. Its rule, lines and severity are the model's — whether a placed rule blocks reads the
// severity, so Canyon never lowers it — its title Canyon's, `Any signed-in person can reach this route`, the model's
// own title the problem's first sentence so what it named stays (#2890), its required_end_state
// and do_not Canyon's own where it blocks and none where it does not — the model's would ask for the
// login again — and a candidate the profile refuses is still dropped. Another rule's words, and every
// word on a host with auth off, stay the model's (the QA batch, item 3).
func TestAnUnauthenticatedRouteBehindCanyonsSignInSaysUseTheIdentity(t *testing.T) {
	auth, perf, blank := candidate(), candidate(), candidate()
	auth.Rule, auth.Severity, auth.Title = "auth.unauthenticated-route", "critical", "GET /claims serves every claim with no sign-in check"
	auth.Problem, auth.Fix = "Anonymous callers read every claim.", "Add Django's login: accounts, a database and a login page."
	auth.RequiredEndState, auth.DoNot = "The route answers only a request with a session cookie.", []string{"read a header the caller sets"}
	perf.StartLine, perf.EndLine = 63, 63
	blank.Rule, blank.StartLine, blank.EndLine = "auth.unauthenticated-route", 62, 62
	for _, c := range []struct {
		signedIn bool
		state    policy.State
	}{{true, policy.Advisory}, {true, policy.Blocking}, {false, policy.Advisory}} {
		r := reviewer(t, &model{resp: Response{Results: []Candidate{auth, perf, blank}}})
		r.SignedIn = c.signedIn
		snap, err := policy.WithStates(r.Snapshot, map[string]policy.State{"auth.unauthenticated-route": c.state})
		if err != nil {
			t.Fatal(err)
		}
		r.Snapshot = snap
		_, scanned := golden(t)
		out, err := r.Review(context.Background(), claimsPortal, scanned)
		if err != nil {
			t.Fatal(err)
		}
		mustSchemas(t, out)
		got := out.Runs[1].Results
		if len(got) != 2 {
			t.Fatalf("%+v: %d results, want the blank line dropped", c, len(got))
		}
		a, p := got[0].Properties.Canyon, got[1].Properties.Canyon
		want := finding.Canyon{Title: auth.Title, Problem: auth.Problem, Fix: auth.Fix, RequiredEndState: auth.RequiredEndState, DoNot: auth.DoNot}
		if c.signedIn {
			want.Title = "Any signed-in person can reach this route"
			want.Problem = auth.Title + ". Canyon signs people in before they reach this app, so only people with access to it reach this route; the route itself does not check who is calling."
			want.Fix = "Don't build a login of your own: Canyon already signs people in. Where this route needs to know or check who is calling, read the identity token Canyon sends with every request in the Authorization: Bearer header, verify it with CANYON_IDENTITY_ISSUER, CANYON_IDENTITY_AUDIENCE and CANYON_IDENTITY_JWKS_URL, and use its sub, email and name. A destructive or admin-only route checks a role; an ordinary write records who did it, from the identity."
			want.RequiredEndState, want.DoNot = "", nil
			if c.state == policy.Blocking {
				want.RequiredEndState = "Canyon's review no longer reports auth.unauthenticated-route for this code when it runs again, and the app still does what it did."
				want.DoNot = []string{"Silence it with an ignore comment or a suppression pragma — Canyon does not read those.", "Remove or rename the flagged code just so the review stops matching it."}
			}
		}
		if got[0].RuleID != auth.Rule || a.Title != want.Title || a.Severity != finding.Critical || a.Problem != want.Problem || a.Fix != want.Fix ||
			a.RequiredEndState != want.RequiredEndState || !slices.Equal(a.DoNot, want.DoNot) {
			t.Errorf("%+v: %s %+v", c, got[0].RuleID, a)
		}
		if p.Problem != perf.Problem || p.Fix != perf.Fix {
			t.Errorf("%+v: another rule's words changed: %+v", c, p)
		}
	}
}
