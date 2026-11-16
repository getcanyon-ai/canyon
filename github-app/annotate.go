package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/runs"
)

// maxAnnotations is how many findings a check carries; its summary counts the rest.
const maxAnnotations = 200

// verdictSQL is the run's stored verdict as the run read has it (routes_gate.go): counts and results in log
// order. Nothing else is read, never the source: the gate stored it redacted, so no scanner's input reaches GitHub.
const verdictSQL = `SELECT COALESCE((t.data->>'blocking')::int, 0), COALESCE((t.data->>'warnings')::int, 0),
  (SELECT COALESCE(jsonb_agg(f.finding ORDER BY f.n), '[]') FROM jsonb_path_query(t.data, '$.findings.runs[*].results[*]') WITH ORDINALITY f(finding, n))
FROM timeline_rows t WHERE t.run_id = $1 AND t.kind = 'verdict'`

// result is the part of a finding an annotation is made of; every result has one location with a full
// region (contracts/finding.md). No columns: SARIF counts code points, GitHub does not.
type result struct {
	Locations []struct {
		PhysicalLocation struct {
			ArtifactLocation struct{ URI string }
			Region           struct{ StartLine, EndLine int }
		}
	}
	Properties struct {
		Canyon struct {
			Severity, Title, Problem, Fix string
			Blocks                        bool
			RequiredEndState              string `json:"required_end_state"`
		}
	}
}

// concluded is how an ended run ends the check: clean succeeds, blocked fails with its counts, failed
// fails with its reason; the verdict's findings are the check's annotations, the first 200 of them.
func concluded(ctx context.Context, pool *pgxpool.Pool, end ending, state, reason, step string) (ending, error) {
	if state == "failed" {
		end.Conclusion, end.Title, end.Summary = "failure", "The review didn't finish", "Canyon's review of "+end.Package+" didn't finish: "+events.FailureWords(reason, step)+"."
		return end, nil
	}
	var blocking, warnings int
	var results []result
	if err := pool.QueryRow(ctx, verdictSQL, end.Run).Scan(&blocking, &warnings, &results); err != nil {
		return ending{}, err
	}
	end.Conclusion, end.Title = "success", "No blocking findings"
	if state == "blocked" {
		var project string
		var findings json.RawMessage
		if err := pool.QueryRow(ctx, "SELECT r.project_id, t.data->'findings' FROM runs r JOIN timeline_rows t ON t.run_id = r.id AND t.kind = 'verdict' WHERE r.id = $1", end.Run).Scan(&project, &findings); err != nil {
			return ending{}, err
		}
		_, unapproved, err := runs.Approvals(ctx, pool, project, end.Run, findings)
		if err != nil {
			return ending{}, err
		}
		end.Conclusion, end.Title = "failure", plural(blocking, "blocking finding")
		if unapproved == 0 { // the org approved each, so a deploy of it goes on: the check does not block the merge either (#996)
			end.Conclusion, end.Title = "success", "Every blocking finding approved"
		}
	}
	end.Summary = fmt.Sprintf("Canyon reviewed %s: %s, %s.", end.Package, strings.ToLower(end.Title[:1])+end.Title[1:], plural(warnings, "warning"))
	if more := len(results) - maxAnnotations; more > 0 {
		end.Summary += fmt.Sprintf(" …and %d more findings in Canyon.", more)
	}
	for _, r := range results[:min(len(results), maxAnnotations)] {
		loc, c := r.Locations[0].PhysicalLocation, r.Properties.Canyon
		level := "notice"
		switch {
		case c.Blocks:
			level = "failure"
		case c.Severity == "critical" || c.Severity == "high":
			level = "warning"
		}
		end.Annotations = append(end.Annotations, Annotation{Path: loc.ArtifactLocation.URI, StartLine: loc.Region.StartLine, EndLine: loc.Region.EndLine,
			AnnotationLevel: level, Title: c.Title, Message: c.Problem, RawDetails: strings.TrimSpace(c.Fix + "\n\n" + c.RequiredEndState)})
	}
	return end, nil
}

// plural is n noun, or n nouns.
func plural(n int, noun string) string {
	if n != 1 {
		noun += "s"
	}
	return fmt.Sprintf("%d %s", n, noun)
}
