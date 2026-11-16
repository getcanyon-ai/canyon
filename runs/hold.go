package runs

import (
	"cmp"
	"context"
	"errors"
	"strings"
	"time"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/jackc/pgx/v5"
)

// ErrHeld is a production write while the app's production is held (contracts/routes.md, The hold): 409 held.
var ErrHeld = errors.New("runs: production is held")

// Hold is an app's held production: who held it, when and why; what production ran then — the package,
// the image, its gate run and who deployed it, "" and the zero actor when it ran nothing; and the app's
// team then, its owner and contributor addresses, their subjects and groups (design/PERMISSIONS.md: none
// of them lifts it).
type Hold struct {
	By                                   events.Actor
	At                                   time.Time
	Reason                               string
	Package, Image, GateRun              string
	DeployedBy                           events.Actor
	TeamEmails, TeamSubjects, TeamGroups []string
}

// ReadHold is the app's hold, nil when production is not held. An error is a hold that cannot be read, and
// the caller fails closed on it: nothing that writes production goes ahead.
func ReadHold(ctx context.Context, q querier, project string) (*Hold, error) {
	var h Hold
	err := q.QueryRow(ctx, `SELECT held_by_kind, held_by_subject, held_by_display, held_at, reason, coalesce(package_id, ''), coalesce(image, ''), coalesce(gate_run, ''),
		coalesce(deployed_by_kind, ''), coalesce(deployed_by_subject, ''), coalesce(deployed_by_display, ''), team_emails, team_subjects, team_groups
		FROM prod_holds WHERE project_id = $1`, project).Scan(&h.By.Kind, &h.By.Subject, &h.By.Display, &h.At, &h.Reason, &h.Package, &h.Image, &h.GateRun,
		&h.DeployedBy.Kind, &h.DeployedBy.Subject, &h.DeployedBy.Display, &h.TeamEmails, &h.TeamSubjects, &h.TeamGroups)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &h, nil
}

// held is ErrHeld while the app's production is held, read on the caller's transaction.
func held(ctx context.Context, q querier, project string) error {
	h, err := ReadHold(ctx, q, project)
	if err == nil && h != nil {
		return ErrHeld
	}
	return err
}

// Line is the hold's sentence (design/EXPERIENCE.md D5): the team's banner, the 409's message and the line
// a deploy run writes while production is held (contracts/events.md).
func (h Hold) Line() string {
	return "Production was stopped by " + cmp.Or(h.By.Display, h.By.Subject) + " on " + h.At.UTC().Format(time.DateOnly) + ": " + ended(h.Reason) +
		" It stays stopped until a responder or an admin lets it run again."
}

// ended is a hold's reason as the held sentence ends it (design/EXPERIENCE.md D5): a full stop after it, unless it
// already ends a sentence, which read "while we look.. It stays stopped" (#2723).
func ended(reason string) string {
	if strings.HasSuffix(reason, ".") || strings.HasSuffix(reason, "?") || strings.HasSuffix(reason, "!") {
		return reason
	}
	return reason + "."
}

// heldStopLine is a run's last line when a hold met it mid-switch and it could not stop production
// (design/OPERATIONS.md §4).
func heldStopLine(version, run string) string {
	return "production was held while " + version + " was starting; Canyon could not stop it: an admin can look up run " + run
}

// heldLine writes the hold's line on a deploy run while the app's production is held, once.
func (p *Pipeline) heldLine(ctx context.Context, in Input) error {
	return p.write(ctx, func(tx pgx.Tx) error {
		h, err := ReadHold(ctx, tx, in.Project)
		if err != nil || h == nil {
			return err
		}
		_, err = events.AppendRunTx(ctx, tx, in.Run, in.Project, events.BuildLog, "held", map[string]any{"line": h.Line()})
		return err
	})
}
