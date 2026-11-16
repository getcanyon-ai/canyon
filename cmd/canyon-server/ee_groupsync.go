//go:build !noee

package main

import (
	"github.com/getcanyon-ai/canyon/ee/groupsync"
	"github.com/getcanyon-ai/canyon/vault"
)

// Canyon Enterprise's access by sign-in group (ee/groupsync): adding and widening a group grant, and setting another
// groups claim, under the licence.
func init() {
	eeFeatures = append(eeFeatures, func(h *eeHooks, _ *vault.Vault) { h.groupSync = groupsync.New(h.licence) })
}
