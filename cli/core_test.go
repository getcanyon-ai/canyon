package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func cursor(id int64) *int64 { return &id }

// serving answers each page in turn, repeating the last, and records the query it was asked.
func serving(t *testing.T, pages []timelinePage, asked *[]string) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*asked = append(*asked, r.URL.RawQuery)
		body, err := json.Marshal(pages[min(len(*asked)-1, len(pages)-1)])
		if err != nil {
			t.Error(err)
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return newClient(server.URL)
}

// The poll keeps its place with the cursor the server answered, sleeps only when a page came
// back short, and ends on next: null and nothing else — an empty page answers the same cursor.
func TestThePollFollowsTheServersCursorToTheEndOfTheRun(t *testing.T) {
	full := make([]timelineRow, pollLimit)
	for i := range full {
		full[i] = timelineRow{ID: int64(41 + i), TS: "2026-09-02T09:31:04Z", Kind: "build.log", Data: json.RawMessage(`{"line":"x"}`)}
	}
	pages := []timelinePage{
		{Rows: full, Next: cursor(90)},
		{Rows: []timelineRow{{ID: 91, TS: "2026-09-02T09:31:09Z", Kind: "weather.forecast", Data: json.RawMessage(`{}`)}}, Next: cursor(91)},
		{Rows: []timelineRow{}, Next: cursor(91)},
		{Rows: []timelineRow{{ID: 92, TS: "2026-09-02T09:31:11Z", Kind: "verdict", Data: json.RawMessage(`{"state":"blocked"}`)}}, Next: nil},
	}
	var asked []string
	var got []timelineRow
	slept := 0
	clk := clock{now: time.Now, sleep: func(time.Duration) { slept++ }}
	seen, err := poll(context.Background(), serving(t, pages, &asked), "gr_1", clk, checkCeiling, func(r timelineRow) { got = append(got, r) })
	want := []string{"after=0&limit=50", "after=90&limit=50", "after=91&limit=50", "after=91&limit=50"}
	if err != nil || !slices.Equal(asked, want) {
		t.Fatalf("the poll asked %q and ended with %v; want %q", asked, err, want)
	}
	if seen.StartedAt != "2026-09-02T09:31:04Z" || seen.Unknown != 1 || slept != 2 {
		t.Errorf("%+v after %d sleeps; want the first row's ts, 1 unknown kind and 2 sleeps (the full page waits for nothing)", seen, slept)
	}
	if len(got) != pollLimit+2 || string(got[0].Data) != `{"line":"x"}` || string(got[len(got)-1].Data) != `{"state":"blocked"}` {
		t.Errorf("the renderer was handed %d rows, first %s, last %s", len(got), got[0].Data, got[len(got)-1].Data)
	}
}

func TestThePollGivesUpRatherThanWaitForever(t *testing.T) {
	var asked []string
	c := serving(t, []timelinePage{{Rows: []timelineRow{}, Next: cursor(7)}}, &asked) // a run that never ends
	now := time.Now()
	clk := clock{now: func() time.Time { return now }, sleep: func(d time.Duration) { now = now.Add(d) }}
	if _, err := poll(context.Background(), c, "gr_1", clk, checkCeiling, nil); err == nil || !strings.Contains(err.Error(), checkCeiling.String()) {
		t.Errorf("the poll ended with %v", err)
	}
	var over []string
	done := serving(t, []timelinePage{{Rows: []timelineRow{{ID: 41, TS: "2026-09-02T09:31:04Z", Kind: "verdict"}}, Next: nil}}, &over)
	if seen, err := poll(context.Background(), done, "gr_1", wallClock, checkCeiling, nil); err != nil || seen.StartedAt == "" {
		t.Errorf("the clock the binary ships with, on a run already over: %+v, %v", seen, err)
	}
}

// The CLI's catalogue is the schema's run-scope enum. Read, never imported: the events package
// pulls pgx, and this is a CLI. contracts/validate.py ties the schema to events.md.
func TestTheKindCatalogueIsTheSchemasRunScopeEnum(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "contracts", "schema", "timeline-row.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		AllOf []struct {
			If struct {
				Properties struct{ Scope struct{ Const string } }
			}
			Then struct {
				Properties struct{ Kind struct{ Enum []string } }
			}
		}
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	for _, a := range schema.AllOf {
		if a.If.Properties.Scope.Const != "run" {
			continue
		}
		if enum := a.Then.Properties.Kind.Enum; !slices.Equal(runKinds, enum) {
			t.Fatalf("the CLI counts %v as known; the schema's run scope is %v", runKinds, enum)
		}
		return
	}
	t.Fatal("the schema names no run-scope kinds")
}

// A package goes up as plain tar under a length the server can read, with no Origin — this is a
// CLI, not a browser — and only the status the caller asked for is an answer.
func TestWhatTheClientSendsAndWhatItTakesBack(t *testing.T) {
	var method, ctype, origin string
	var length int64
	var sent []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, ctype, origin, length = r.Method, r.Header.Get("Content-Type"), r.Header.Get("Origin"), r.ContentLength
		sent, _ = io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/elsewhere":
			w.WriteHeader(http.StatusOK) // an answer, but not the one that was asked for
		case "/busy":
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":"run_in_progress","message":"a run of this project is running","run":{"id":"gr_9","state":"running"}}`)
			return
		default:
			w.WriteHeader(http.StatusCreated)
		}
		_, _ = io.WriteString(w, `{"package":{"id":"pkg_1"}}`)
	}))
	defer server.Close()
	c, ctx := newClient(server.URL), context.Background()
	var pkg struct{ Package struct{ ID string } }
	tar := io.LimitReader(strings.NewReader(strings.Repeat("x", 4096)), 4096) // not a reader net/http measures for itself
	if err := c.upload(ctx, "/packages", tar, 4096, "", false, &pkg); err != nil {
		t.Fatalf("the upload answered %v", err)
	}
	if method != http.MethodPost || ctype != "application/x-tar" || origin != "" || length != 4096 || len(sent) != 4096 || pkg.Package.ID != "pkg_1" {
		t.Errorf("%s %q, Origin %q, Content-Length %d for %d bytes, answered %+v", method, ctype, origin, length, len(sent), pkg)
	}
	var notCanyon *NotCanyon
	if err := c.postJSON(ctx, "/elsewhere", struct{}{}, http.StatusCreated, &pkg); !errors.As(err, &notCanyon) {
		t.Errorf("a 200 where a 201 was asked for answered %v", err)
	}
	if ctype != "application/json" || string(sent) != "{}" {
		t.Errorf("postJSON sent %q as %q", sent, ctype)
	}
	// A refusal's evidence is the rest of its envelope, and the verb that knows the code reads it.
	var api *APIError
	err := c.postJSON(ctx, "/busy", struct{}{}, http.StatusAccepted, &pkg)
	if !errors.As(err, &api) || api.Code != "run_in_progress" || string(api.Extra["run"]) != `{"id":"gr_9","state":"running"}` {
		t.Errorf("the 409 came back as %+v", api)
	}
}

// The CLI's finding is the seam's result: the golden log decodes into it field for field, so a
// change in contracts/testdata is a red here and not a surprise in a renderer.
func TestTheGoldenFindingDecodesIntoTheTypeCheckHandsOn(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "contracts", "testdata", "finding", "claims-portal.sarif.json"))
	if err != nil {
		t.Fatal(err)
	}
	var log struct {
		Runs []struct {
			Results []finding `json:"results"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(raw, &log); err != nil || len(log.Runs) != 3 || len(log.Runs[0].Results) != 0 || len(log.Runs[1].Results) != 3 || len(log.Runs[2].Results) != 1 {
		t.Fatalf("the golden log did not decode: %v", err)
	}
	first := log.Runs[2].Results[0] // gitleaks' key; the reviewer's run holds the two routes and the refetch
	where, bag := first.Locations[0].PhysicalLocation, first.Properties.Canyon
	blocks, said := first.Blocking()
	if first.RuleID != "secrets.client-bundle.gitleaks.stripe-access-token" || !blocks || !said || bag.Severity != "critical" || bag.SecretName == nil {
		t.Errorf("the bag: %+v", bag)
	}
	if where.ArtifactLocation.URI != "apps/web/src/lib/payments.ts" || where.Region.StartLine != 14 ||
		where.Region.StartColumn != 1 || where.Region.EndLine != 14 || where.Region.EndColumn != 65 || where.Region.Snippet.Text != "••••" {
		t.Errorf("the span: %+v", where)
	}
	if first.Message.Text == "" || bag.Title == "" || bag.Problem == "" || bag.Fix == "" || bag.PolicyName == "" ||
		!strings.HasPrefix(first.PartialFingerprints.CanyonV1, "f_") {
		t.Errorf("a field the profile requires came back empty: %+v", first)
	}
	if blocks, said := log.Runs[1].Results[0].Blocking(); blocks || !said {
		t.Errorf("the golden's high advisory does not block, and carries blocks: false to say so")
	}
}

// Absent is not false. A host that says nothing about blocking has to be told apart from one
// that says no, or a critical finding nobody classified ships as an advisory.
func TestAFindingThatSaysNothingAboutBlockingIsNotOneThatSaysNo(t *testing.T) {
	var silent, refused finding
	if json.Unmarshal([]byte(`{"properties":{"canyon":{"severity":"critical"}}}`), &silent) != nil ||
		json.Unmarshal([]byte(`{"properties":{"canyon":{"severity":"high","blocks":false}}}`), &refused) != nil {
		t.Fatal("the fixtures did not decode")
	}
	if blocks, said := silent.Blocking(); blocks || said {
		t.Errorf("a finding carrying no blocks answered %v, %v; the host said nothing, and critical is not an answer", blocks, said)
	}
	if blocks, said := refused.Blocking(); blocks || !said {
		t.Errorf("a finding carrying blocks: false answered %v, %v", blocks, said)
	}
}

// A full page whose cursor did not move is a host stalling, not a host ahead of us: wait, do not spin.
func TestAFullPageThatEchoesItsCursorWaits(t *testing.T) {
	full := make([]timelineRow, pollLimit)
	for i := range full {
		full[i] = timelineRow{ID: int64(1 + i), TS: "2026-09-02T09:31:04Z", Kind: "build.log", Data: json.RawMessage(`{}`)}
	}
	pages := []timelinePage{{Rows: full, Next: cursor(0)}, {Rows: full, Next: cursor(0)}, {Rows: []timelineRow{}, Next: nil}}
	var asked []string
	slept := 0
	clk := clock{now: time.Now, sleep: func(time.Duration) { slept++ }}
	if _, err := poll(context.Background(), serving(t, pages, &asked), "gr_1", clk, checkCeiling, nil); err != nil || slept != 2 {
		t.Errorf("a stalling host was asked %q with %d sleeps and %v; want a sleep after each echoed page", asked, slept, err)
	}
}

// One byte past the cap tells a cut answer from a whole one; a cut answer is refused, never decoded.
func TestAnAnswerPastTheCapIsRefusedNotCut(t *testing.T) {
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"pad":%q}`, strings.Repeat("x", 2<<20))
	}))
	t.Cleanup(big.Close)
	var v map[string]string
	err := newClient(big.URL).getUpTo(context.Background(), "/api/v1/x", 1<<20, &v)
	if err == nil || !strings.Contains(err.Error(), "more than 1 MiB") {
		t.Errorf("a 2 MiB answer under a 1 MiB cap gave %v", err)
	}
}
