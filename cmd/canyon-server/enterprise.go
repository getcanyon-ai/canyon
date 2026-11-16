package main

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/internal/enterprise"
	"github.com/getcanyon-ai/canyon/vault"
)

// eeHooks is Canyon Enterprise's code as this server was built with it (design/ARCHITECTURE.md §8): each hook set by
// a wiring file of a build with ee/ (ee_<feature>.go, //go:build !noee), nil in a build without it, where every
// Enterprise change answers as a server with no licence does (contracts/routes.md, Licence). ee/ satisfies each
// interface without naming it, so no core package imports ee/. Each feature's move to ee/ adds its own hook here,
// with the code that calls it.
type eeHooks struct {
	licence   licenceHook
	groupSync groupSyncHook
	patching  patchingHook
	outbound  outboundKeys
	injector  injectorOf
}

// licenceHook is the licence: read, required for a feature, and stored, each on the caller's querier or transaction.
type licenceHook interface {
	Read(ctx context.Context, q enterprise.Querier) (enterprise.Licence, error)
	Require(ctx context.Context, q enterprise.Querier, feature string) error
	Store(ctx context.Context, tx pgx.Tx, text string) (enterprise.Stored, error)
	SetByOperator() bool
}

// eeLicence is ee_licence.go's: the server's licence from its environment. nil in a build without Canyon Enterprise.
var eeLicence func(getenv func(string) string) (licenceHook, error)

// eeFeatures are each feature's wiring file's: run once the licence is set, each sets its own hook from it, with the
// server's vault to seal and open what the feature keeps.
var eeFeatures []func(h *eeHooks, keys *vault.Vault)

// wireEnterprise is the server's hooks: the licence first, then each feature's.
func wireEnterprise(getenv func(string) string, keys *vault.Vault) (eeHooks, error) {
	var h eeHooks
	if eeLicence == nil {
		return h, nil
	}
	lic, err := eeLicence(getenv)
	if err != nil {
		return h, err
	}
	h.licence = lic
	for _, wire := range eeFeatures {
		wire(&h, keys)
	}
	return h, nil
}

// read is the licence as it stands: none in a build without Canyon Enterprise.
func (h eeHooks) read(ctx context.Context, q querier) (enterprise.Licence, error) {
	if h.licence == nil {
		return enterprise.Licence{State: "none", Allows: []string{}}, nil
	}
	return h.licence.Read(ctx, q)
}

// require is nil when a licence in force unlocks feature, else licenceErr, always in a build without Canyon
// Enterprise; an error reading it is the caller's 500.
func (h eeHooks) require(ctx context.Context, q querier, feature string) error {
	if h.licence == nil {
		return licenceErr{Feature: feature}
	}
	return h.licence.Require(ctx, q, feature)
}
