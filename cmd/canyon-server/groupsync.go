package main

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// groupSyncHook is Canyon Enterprise's access by sign-in group (ee/groupsync; design/ARCHITECTURE.md §8): it adds or
// widens a group grant and sets another groups claim, each under a licence naming group_sync. The core never writes
// either, so without it no group gains access; what is set keeps granting, and narrowing, removing and clearing stay
// the core's (contracts/routes.md, Licence, A server built without Canyon Enterprise).
type groupSyncHook interface {
	Grant(ctx context.Context, tx pgx.Tx, project, group, level string, roles []string, by actor) error
	SetClaim(ctx context.Context, tx pgx.Tx, claim string) error
}

// grantGroup adds or widens group's grant on project, or is licenceErr in a build without Canyon Enterprise.
func (h eeHooks) grantGroup(ctx context.Context, tx pgx.Tx, project, group, level string, roles []string, by actor) error {
	if h.groupSync == nil {
		return licenceErr{Feature: "group_sync"}
	}
	return h.groupSync.Grant(ctx, tx, project, group, level, roles, by)
}

// setGroupsClaim sets another groups claim, or is licenceErr in a build without Canyon Enterprise.
func (h eeHooks) setGroupsClaim(ctx context.Context, tx pgx.Tx, claim string) error {
	if h.groupSync == nil {
		return licenceErr{Feature: "group_sync"}
	}
	return h.groupSync.SetClaim(ctx, tx, claim)
}
