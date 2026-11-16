// Package groupsync is Canyon Enterprise's access by sign-in group (contracts/routes.md, Licence; design/PERMISSIONS.md,
// Licence): adding or widening an app's group grant, and setting the claim a sign-in reads a person's groups by, each
// only under a licence in force that names group_sync. Reading the groups at sign-in, enforcing a grant, narrowing and
// removing one, and clearing the claim are the core's (design/ARCHITECTURE.md §8).
package groupsync

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/internal/enterprise"
)

const feature = "group_sync"

// licence is the licence a change is judged by: enterprise.Required when none in force names the feature.
type licence interface {
	Require(ctx context.Context, q enterprise.Querier, feature string) error
}

// GroupSync adds and widens group grants and sets the groups claim, under lic.
type GroupSync struct{ lic licence }

// New is group sync under lic.
func New(lic licence) *GroupSync { return &GroupSync{lic: lic} }

// Grant writes group's grant on project whole, at level with roles, granted by by: a grant the group does not hold, or
// one it widens. The caller holds the ledger lock and has judged that it widens.
func (g *GroupSync) Grant(ctx context.Context, tx pgx.Tx, project, group, level string, roles []string, by events.Actor) error {
	if err := g.lic.Require(ctx, tx, feature); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO project_access (project_id, kind, grantee, level, roles, granted_by_kind, granted_by_subject, granted_by_display)
		VALUES ($1, 'group', $2, $3, $4, $5, $6, $7)
		ON CONFLICT (project_id, kind, grantee) DO UPDATE SET level = EXCLUDED.level, roles = EXCLUDED.roles`,
		project, group, level, roles, by.Kind, by.Subject, by.Display)
	return err
}

// SetClaim sets the groups claim to claim, one other than the stored one.
func (g *GroupSync) SetClaim(ctx context.Context, tx pgx.Tx, claim string) error {
	if err := g.lic.Require(ctx, tx, feature); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "UPDATE org_preferences SET groups_claim = $1", claim)
	return err
}
