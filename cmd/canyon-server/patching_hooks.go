package main

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/runs"
)

// patchingHook is platform patching's Canyon Enterprise code (ee/patching; design/ARCHITECTURE.md §8): what decides
// and schedules Canyon's patches, which the pipeline runs (runs.Patcher), and the writes that turn patching on,
// approve an identity for the fleet and resume one, each in the caller's transaction once it has refused all else.
// nil in a server built without it, which reads patching as off and refuses each of those writes 409
// licence_required (contracts/routes.md, Licence).
type patchingHook interface {
	runs.Patcher
	TurnOn(ctx context.Context, tx pgx.Tx, patching, rebuilds bool) error
	Approve(ctx context.Context, tx pgx.Tx, identity string, by actor, email, reason string) error
	Resume(ctx context.Context, tx pgx.Tx, identity string) error
}

// platformPatching is platform patching's name in a licence and in licence_required (contracts/routes.md, Licence).
const platformPatching = "platform_patching"

// turnPatchingOn turns the org's patching, rebuilds or both to ask in tx, once a licence in force names
// platform_patching; licenceErr in a server built without Canyon Enterprise.
func (h eeHooks) turnPatchingOn(ctx context.Context, tx pgx.Tx, patching, rebuilds bool) error {
	if h.patching == nil {
		return licenceErr{Feature: platformPatching}
	}
	return h.patching.TurnOn(ctx, tx, patching, rebuilds)
}
