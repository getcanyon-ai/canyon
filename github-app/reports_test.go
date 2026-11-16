package githubapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/events"
	gatetree "github.com/getcanyon-ai/canyon/gate"
	"github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/gate/sarif"
	"github.com/getcanyon-ai/canyon/runs"
	"github.com/jackc/pgx/v5"
)

// pushSHA is push_default_branch.json's commit; other is another commit's.
var pushSHA, other = "a1b2c3d4e5f60718293a4b5c6d7e8f9001122334", strings.Repeat("b", 40)

// scan is claims-portal's code scanning as a test sets it, and what the courier did with it.
type scan struct {
	status   int      // the list's answer, when not 200
	list     []string // the analyses listed
	refuse   bool     // the attach refuses
	slow     bool     // a download stalls after its headers
	sarif    string   // every download's body, when set
	queries  []string // each list page's query
	accepts  []string // each SARIF download's Accept
	attached []string // each attach: project, package, actor and the SARIF
	vias     []string // each attach's channel, as its audit row would record it
	log      strings.Builder
}

// codeScanning is installed with codeql blocking and semgrep off, the list 100 to a page, each analysis's SARIF {"analysis":<id>}.
func codeScanning(t *testing.T) (chan string, *scan) {
	t.Helper()
	calls, sc := installed(t), &scan{}
	courier.Log = log.New(&sc.log, "", 0)
	t.Cleanup(func() { courier.Log = log.New(io.Discard, "", 0) })
	courier.Policy = func(context.Context, pgx.Tx, string) (policy.Snapshot, error) {
		return policy.Snapshot{Limits: policy.Limits{GateRunsPerProjectPerHour: 30, GateRunsPerActorPerHour: 60}, Rules: []policy.Rule{
			{ID: "sarif.codeql", Name: "CodeQL", State: policy.Blocking, Producer: policy.Sarif},
			{ID: "sarif.semgrep", Name: "Semgrep", State: policy.Off, Producer: policy.Sarif},
		}}, nil
	}
	courier.Attach = func(ctx context.Context, project, pkg string, raw []byte, by events.Actor) error {
		sc.attached = append(sc.attached, strings.Join([]string{project, pkg, by.Kind, by.Subject, by.Display, string(raw)}, " "))
		sc.vias = append(sc.vias, events.ProvenanceOf(ctx).Via)
		if sc.refuse {
			return errors.New("the report is not exactly one run")
		}
		return nil
	}
	hook(func() {
		scanning = func(w http.ResponseWriter, r *http.Request) {
			if id, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/repos/acme/claims-portal/code-scanning/analyses/")); err == nil {
				sc.accepts = append(sc.accepts, r.Header.Get("Accept"))
				if w.WriteHeader(http.StatusOK); sc.slow {
					w.(http.Flusher).Flush()
					time.Sleep(time.Second)
				}
				if sc.sarif != "" {
					_, _ = io.WriteString(w, sc.sarif)
					return
				}
				_, _ = fmt.Fprintf(w, `{"analysis":%d}`, id)
				return
			}
			if sc.queries = append(sc.queries, r.URL.RawQuery); sc.status != 0 {
				http.Error(w, `{"message":"Resource not accessible by integration"}`, sc.status)
				return
			}
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			_, _ = io.WriteString(w, "["+strings.Join(sc.list[min((page-1)*100, len(sc.list)):min(page*100, len(sc.list))], ",")+"]")
		}
	})
	return calls, sc
}

// analysis is one analysis as GitHub lists it, made minutes ago.
func analysis(id int, sha, tool, category string, minutes int) string {
	at := time.Now().Add(-time.Duration(minutes) * time.Minute).UTC().Format(time.RFC3339)
	return fmt.Sprintf(`{"id":%d,"ref":"refs/heads/main","commit_sha":%q,"category":%q,"created_at":%q,"tool":{"name":%q,"guid":null,"version":"2.19.0"}}`, id, sha, category, at, tool)
}

// Of the tools a run imports, the newest analysis per category at the pushed commit, on the pushed branch and
// on any page, is downloaded as SARIF and attached to its package as the pusher, before the review: never
// another commit's, however new.
func TestTheCommitsNewestAnalysisPerCategoryIsAttachedAsThePusher(t *testing.T) {
	calls, sc := codeScanning(t)
	sc.list = append(slices.Repeat([]string{analysis(9, pushSHA, "Trivy", "", 0)}, 150), // not added, and a page and a half of it
		analysis(1, other, "CodeQL", "/language:javascript", 0),
		analysis(2, pushSHA, "CodeQL", "/language:javascript", 1),
		analysis(3, pushSHA, "CodeQL", "/language:javascript", 5),
		analysis(4, pushSHA, "CodeQL", "/language:python", 2),
		analysis(5, pushSHA, "Semgrep", "", 1), // its rule is off
		analysis(6, pushSHA, "Trivy", "", 1),   // not added
	)
	got := pushed(t, calls, "d-scanned")
	var pkg string
	if err := courier.Pool.QueryRow(ctx, "SELECT id FROM packages").Scan(&pkg); err != nil {
		t.Fatal(err)
	}
	by := "claims-portal " + pkg + " agent github:dana-acme dana-acme "
	pages := []string{"page=1&per_page=100&ref=refs%2Fheads%2Fmain", "page=2&per_page=100&ref=refs%2Fheads%2Fmain"}
	if want := []string{by + `{"analysis":2}`, by + `{"analysis":4}`}; !slices.Equal(sc.queries, pages) || !slices.Equal(sc.attached, want) || len(got) != 1 || got[0].Conclusion != "success" {
		t.Fatalf("listed %q, want %q; attached %q, want %q; the check %+v", sc.queries, pages, sc.attached, want, got)
	}
	if !slices.Equal(sc.vias, []string{"github", "github"}) { // the App's work, never Canyon's own
		t.Fatalf("the attaches' channel: %q", sc.vias)
	}
	if want := []string{"application/sarif+json", "application/sarif+json"}; !slices.Equal(sc.accepts, want) {
		t.Fatalf("the downloads asked for %q, want SARIF", sc.accepts)
	}
}

// A tool with analyses but none at the commit is waited for, then is no report, logged; one with none is not
// waited for, nor a pull request's (GitHub analyses its merge commit), and five pages are the most read. GitHub's
// refusal, a stalled download, the attach's refusal and a pusher past the limit, who downloads nothing, are one
// log line each, and the review goes on.
func TestACommitNotYetAnalysedIsWaitedForAndAFailureIsNoReport(t *testing.T) {
	calls, sc := codeScanning(t)
	t.Cleanup(func() { sarifDeadline = 2 * time.Minute })
	sc.list = []string{analysis(1, other, "CodeQL", "/language:javascript", 0), analysis(2, pushSHA, "Semgrep", "", 0)}
	codeql := []string{analysis(3, pushSHA, "CodeQL", "", 0)}
	for _, c := range []struct {
		id, event, fixture        string
		set                       func()
		asks, downloads, attached int
		log, end                  string
	}{
		{"d-waited", "push", "push_default_branch.json", func() {}, reportTries + 1, 0, 0, "codeql: no code-scanning analysis for " + pushSHA + " after 10 minutes\n", "success"},
		{"d-pr", "pull_request", "pull_request_opened.json", func() {}, 1, 0, 0, "", "success"},
		{"d-none", "push", "push_default_branch.json", func() { sc.list = nil }, 1, 0, 0, "", "success"},
		{"d-bounded", "push", "push_default_branch.json", func() {
			sc.list = append(slices.Repeat([]string{analysis(9, pushSHA, "Trivy", "", 0)}, 500), codeql...)
		}, 5, 0, 0, "", "success"},
		{"d-403", "push", "push_default_branch.json", func() { sc.status = http.StatusForbidden }, 1, 0, 0,
			"code scanning not read: github: GET /repos/acme/claims-portal/code-scanning/analyses?page=1&per_page=100&ref=refs%2Fheads%2Fmain answered 403: Resource not accessible by integration\n", "success"},
		{"d-refused", "push", "push_default_branch.json", func() { sc.status, sc.list, sc.refuse = 0, codeql, true }, 1, 1, 1,
			"CodeQL analysis 3 not attached: the report is not exactly one run\n", "success"},
		{"d-stalled", "push", "push_default_branch.json", func() { sc.refuse, sc.slow, sarifDeadline = false, true, 50*time.Millisecond }, 1, 1, 0,
			"CodeQL analysis 3 not attached: context deadline exceeded\n", "success"},
		{"d-limited", "push", "push_default_branch.json", func() {
			courier.Policy = func(context.Context, pgx.Tx, string) (policy.Snapshot, error) {
				return policy.Snapshot{Limits: policy.Limits{GateRunsPerProjectPerHour: 30, GateRunsPerActorPerHour: 1}, Rules: []policy.Rule{{ID: "sarif.codeql", State: policy.Blocking, Producer: policy.Sarif}}}, nil
			}
		}, 1, 0, 0, "dana-acme is past the limit of 1 reviews an hour: no code-scanning report attached\n", "failure"},
	} {
		c.set()
		sc.queries, sc.accepts, sc.attached = nil, nil, nil
		sc.log.Reset()
		edits := []string{`"name":"billing-api"`, `"name":"claims-portal"`}
		if c.event == "pull_request" {
			edits = nil
		}
		if rec := deliver(t, c.event, c.id, c.fixture, edits...); rec.Code != 202 {
			t.Fatalf("%s: %d %s", c.id, rec.Code, rec.Body)
		}
		patch, line := ended(t, calls, c.id), sc.log.String()
		ref := "&ref=refs%2Fheads%2Fmain"
		if c.event == "pull_request" {
			ref = ""
		}
		if len(sc.queries) != c.asks || sc.queries[0] != "page=1&per_page=100"+ref || len(sc.accepts) != c.downloads || len(sc.attached) != c.attached ||
			len(patch) != 1 || !strings.Contains(patch[0], `"conclusion":"`+c.end+`"`) || strings.Count(line, "\n") != min(len(c.log), 1) || !strings.HasSuffix(line, c.log) {
			t.Errorf("%s: listed %q, want %d page(s); %d download(s), want %d; %d attach(es), want %d; logged %q, want …%q; the check %s",
				c.id, sc.queries, c.asks, len(sc.accepts), c.downloads, len(sc.attached), c.attached, line, c.log, patch)
		}
	}
}

// GitHub's category of each analysis is its report's, whatever the SARIF downloaded says: two languages of one
// tool, their SARIF naming no automationDetails.id, are two reports, neither replacing the other.
func TestEachCategoryIsItsOwnReportThoughTheSARIFNamesNone(t *testing.T) {
	calls, sc := codeScanning(t)
	sc.sarif = `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"CodeQL"}},"results":[]}]}`
	sc.list = []string{analysis(2, pushSHA, "CodeQL", "/language:javascript", 1), analysis(4, pushSHA, "CodeQL", "/language:python", 2)}
	snap, _ := courier.Policy(ctx, nil, "")
	var replaced []string
	courier.Attach = func(ctx context.Context, project, pkg string, raw []byte, by events.Actor) error { // the route's core, less the package's tree: these reports have no results
		rep, err := sarif.Normalize(raw, &snap, gatetree.Tree{})
		if err != nil {
			return err
		}
		tx, err := courier.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		sum := sha256.Sum256(raw)
		_, was, err := runs.AttachReportTx(ctx, tx, project, pkg, runs.Report{Report: rep, Digest: "sha256:" + hex.EncodeToString(sum[:]), AttachedBy: by})
		replaced = append(replaced, was)
		if err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	pushed(t, calls, "d-matrix")
	var categories string
	if err := courier.Pool.QueryRow(ctx, "SELECT string_agg(tool || ' ' || category, ', ' ORDER BY id) FROM package_reports").Scan(&categories); err != nil {
		t.Fatal(err)
	}
	if want := "codeql language-javascript, codeql language-python"; categories != want || !slices.Equal(replaced, []string{"", ""}) {
		t.Fatalf("stored %q, want %q; replaced %q, want none", categories, want, replaced)
	}
}

// A tool Off before preview and placed before production is on: a push imports its analyses
// (contracts/snapshot.md, Two gates); one Off in both gates is not imported.
func TestAToolPlacedBeforeProductionAloneIsImported(t *testing.T) {
	installed(t)
	was := courier.Policy
	t.Cleanup(func() { courier.Policy = was })
	courier.Policy = func(context.Context, pgx.Tx, string) (policy.Snapshot, error) {
		return policy.Snapshot{Limits: policy.Limits{GateRunsPerProjectPerHour: 30, GateRunsPerActorPerHour: 60}, Rules: []policy.Rule{
			{ID: "sarif.codeql", Name: "CodeQL", State: policy.Off, Production: policy.Blocking, Producer: policy.Sarif},
			{ID: "sarif.semgrep", Name: "Semgrep", State: policy.Off, Producer: policy.Sarif},
		}}, nil
	}
	imp, err := courier.imports(ctx, "claims-portal")
	if err != nil || !slices.Equal(imp.Tools, []string{"codeql"}) {
		t.Fatalf("imported %q (%v), want codeql alone", imp.Tools, err)
	}
}
