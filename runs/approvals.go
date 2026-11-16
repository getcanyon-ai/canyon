package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/jackc/pgx/v5"
)

// Approval is an unexpired approval of one blocking finding, in the approve route's shape
// (contracts/routes.md); a deploy that goes ahead under approvals lists them on its verdict row.
type Approval struct {
	Rule        string       `json:"rule"`
	Fingerprint string       `json:"fingerprint"`
	Reason      string       `json:"reason"`
	ExpiresAt   string       `json:"expires_at"`
	ApprovedBy  events.Actor `json:"approved_by"`
}

// ShipMargin is how long an approval must still stand for it to clear a ship at deploy time (Cleared, and the
// server's judgement of a promotion's review): an approval within it of expiry clears no deploy, whatever slept
// between the check and the container's start, though every read still shows it (contracts/routes.md, the
// approve rows).
const ShipMargin = 10 * time.Minute

// CoversBlocking is the one SQL condition under which approval a covers a blocking finding of a run — run
// the SQL alias of that run's runs row, after the SQL instant it must outlive, now() where it is read and
// now() plus ShipMargin where a ship is judged — wherever an approval is read (contracts/routes.md, the
// approve rows; #2204): filed under the approver rule, unexpired at after, a person's (an approval Canyon's AI wrote
// before it stopped approving covers nothing, contracts/finding.md), and never an author's of the run (Author) —
// the person who started it, or one who linked the GitHub account whose push started it (#2228); of a patch
// run, the author of the run whose image it rebases (contracts/routes.md, Patch runs) — nor any on a run that
// recorded no author, a patch run whose image's run recorded none among them (fail closed); nor, while the org
// asks approvers of a push to link GitHub (push_approvers_linked; #2342), any on a push's run by an approver
// who linked no GitHub account, wherever it was filed. The anonymous person is every caller of a host with
// sign-in off, where nothing is refused; with it on, the server voids its approvals as it starts. Its approval
// alias is a.
func CoversBlocking(run, after string) string {
	kind, subject, github := Author(run, "kind"), Author(run, "subject"), Author(run, "github")
	return "(a.approves_blocking AND a.expires_at > " + after + " AND a.approved_by_kind <> 'canyon' AND " + kind + " IS NOT NULL AND " + kind + " <> 'canyon' AND NOT (a.approved_by_kind = 'person' AND (" +
		"(" + kind + " = 'person' AND a.approved_by_subject = " + subject + " AND a.approved_by_subject <> 'anonymous')" +
		" OR EXISTS (SELECT 1 FROM github_links l WHERE l.github_id = " + github + " AND l.subject = a.approved_by_subject)))" + // a link is never the anonymous person's: only a signed-in person makes one
		" AND (" + github + " IS NULL OR NOT (SELECT push_approvers_linked FROM org_preferences)" +
		" OR (a.approved_by_kind = 'person' AND EXISTS (SELECT 1 FROM github_links l WHERE l.subject = a.approved_by_subject))))"
}

// Author is the SQL for one part of the author of the run the alias run names — kind, subject, display or
// github — as every rule that never lets a run's author approve its findings reads it (design/PERMISSIONS.md,
// Approve findings): who started it, its started_by, or of a patch run, which Canyon started, the author it
// recorded, NULL when it recorded none.
func Author(run, part string) string {
	return "CASE WHEN " + run + ".kind = 'patch' THEN " + run + ".author_" + part + " ELSE " + run + ".started_by_" + part + " END"
}

// Blocks is whether a result blocks before preview as its verdict says: a result that does not say reads as
// blocking (fail closed).
func Blocks(b *bool) bool { return b == nil || *b }

// approvalsSQL is, for each of the log's results in log order — a run's null results are none, as
// json.Unmarshal reads them, never a result of their own — the project's approval of it on (rule,
// fingerprint) that covers it when it blocks (CoversBlocking, against run $3's author), and which results are covered.
var approvalsSQL = `SELECT
  COALESCE(jsonb_agg(jsonb_build_object('rule', a.rule, 'fingerprint', a.fingerprint, 'reason', a.reason,
      'expires_at', to_char(a.expires_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
      'approved_by', jsonb_build_object('kind', a.approved_by_kind, 'subject', a.approved_by_subject, 'display', a.approved_by_display))
    ORDER BY f.n) FILTER (WHERE a.rule IS NOT NULL), '[]'),
  COALESCE(array_agg(a.rule IS NOT NULL ORDER BY f.n), '{}')
FROM jsonb_path_query($2::jsonb, '$.runs[*].results[*] ? (@.type() == "object")') WITH ORDINALITY f(finding, n)
LEFT JOIN runs g ON g.id = $3
LEFT JOIN approvals a ON COALESCE(f.finding->'properties'->'canyon'->'blocks', 'true') <> 'false' AND a.project_id = $1 AND a.rule = f.finding->>'ruleId'
  AND a.fingerprint = f.finding->'partialFingerprints'->>'canyon/v1' AND ` + CoversBlocking("g", "now() + $4::interval")

// Approvals answers, for a SARIF log in Canyon's profile, which approvals cover its blocking findings
// and how many blocking entries are left open, an entry open while any of its findings is uncovered
// (contracts/finding.md, One problem, one entry): the deploy's verdict step and the promote receipt ask
// it the same way, of run, the run gated on them, whose author no approval of their own covers. A nil log
// has no blocking finding.
func Approvals(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, project, run string, findings json.RawMessage) (approved []Approval, unapproved int, err error) {
	return ApprovalsWithin(ctx, q, project, run, findings, 0)
}

// ApprovalsWithin is Approvals with each approval standing margin longer: ShipMargin where a ship is judged.
func ApprovalsWithin(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, project, run string, findings json.RawMessage, margin time.Duration) (approved []Approval, unapproved int, err error) {
	var covered []bool
	if err = q.QueryRow(ctx, approvalsSQL, project, findings, run, margin).Scan(&approved, &covered); err != nil || len(covered) == 0 {
		return approved, 0, err
	}
	var log finding.Log
	if err = json.Unmarshal(findings, &log); err != nil {
		return nil, 0, err
	}
	entries, open, i := finding.Entries(log), map[int]bool{}, 0
	if len(entries) != len(covered) {
		return nil, 0, fmt.Errorf("runs: approvals read %d results of a log of %d", len(covered), len(entries))
	}
	for _, run := range log.Runs {
		for _, r := range run.Results {
			if Blocks(r.Properties.Canyon.Blocks) && !covered[i] {
				open[entries[i]] = true
			}
			i++
		}
	}
	return approved, len(open), nil
}

// Cleared is whether review run clears what a deploy of it ships to preview, read now: its verdict ended
// clean, or blocked with every blocking finding covered for ShipMargin yet (ApprovalsWithin). Fail closed: a run with no verdict, or a
// verdict in any other state, clears nothing.
func Cleared(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, project, run string) (bool, error) {
	var state string
	var findings json.RawMessage
	err := q.QueryRow(ctx, "SELECT COALESCE(data->>'state', ''), data->'findings' FROM timeline_rows WHERE run_id = $1 AND kind = 'verdict'", run).Scan(&state, &findings)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil || state != "clean" && state != "blocked":
		return false, err
	case state == "clean":
		return true, nil
	}
	_, open, err := ApprovalsWithin(ctx, q, project, run, findings, ShipMargin)
	return err == nil && open == 0, err
}
