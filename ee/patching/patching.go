// Package patching is Canyon Enterprise's platform patching (design/PATCHING.md §4–§5; design/ARCHITECTURE.md §8):
// what decides and schedules Canyon's patches — each rescan tick's rebase and rebuild plans and their waves, filing
// patch approvals and the fleet approval's decisions — and turning patching on. The patch run itself, the rescan,
// the reads and every refusal are the core's (runs, cmd/canyon-server).
package patching

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/internal/enterprise"
)

// Feature is platform patching's name in a licence (contracts/routes.md, Licence).
const Feature = "platform_patching"

// requirer is the licence, as Canyon Enterprise's ee/licence answers it.
type requirer interface {
	Require(ctx context.Context, q enterprise.Querier, feature string) error
}

// Patching is the server's platform patching, unlocked by its licence.
type Patching struct{ licence requirer }

// New is platform patching under licence.
func New(licence requirer) Patching { return Patching{licence: licence} }

// TurnOn turns the org's patching, rebuilds or both to ask in tx, once a licence in force names platform_patching:
// else enterprise.Required, nothing written (contracts/routes.md, Licence). The caller has refused everything else
// first, and writes off itself.
func (pt Patching) TurnOn(ctx context.Context, tx pgx.Tx, patching, rebuilds bool) error {
	if err := pt.licence.Require(ctx, tx, Feature); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE org_preferences SET patching = CASE WHEN $1 THEN 'ask' ELSE patching END, rebuilds = CASE WHEN $2 THEN 'ask' ELSE rebuilds END`, patching, rebuilds)
	return err
}

// Approve is the fleet approval's write in tx (contracts/routes.md, the fleet approval): identity, which the caller
// holds locked and found not yet approved, approved by by, signed in as email, for reason.
func (Patching) Approve(ctx context.Context, tx pgx.Tx, identity string, by events.Actor, email, reason string) error {
	_, err := tx.Exec(ctx, `UPDATE patch_identities SET approved_by_kind = $2, approved_by_subject = $3, approved_by_display = $4, approved_at = now(), approved_reason = $5,
		approved_by_email = $6 WHERE id = $1`, identity, by.Kind, by.Subject, by.Display, reason, email)
	return err
}

// Resume is a resume's write in tx (contracts/routes.md, the identities' resume): identity, which the caller holds
// locked and found paused or stalled, goes on from the wave it stopped in, and its breaker counts afresh.
func (Patching) Resume(ctx context.Context, tx pgx.Tx, identity string) error {
	_, err := tx.Exec(ctx, `UPDATE patch_identities SET paused_at = NULL, paused_projects = NULL, stalled_at = NULL, stalled_projects = NULL, counted_since = now()
		WHERE id = $1`, identity)
	return err
}
