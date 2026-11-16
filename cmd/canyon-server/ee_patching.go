//go:build !noee

package main

import (
	"github.com/getcanyon-ai/canyon/ee/patching"
	"github.com/getcanyon-ai/canyon/vault"
)

// Canyon Enterprise's platform patching (ee/patching), unlocked by the licence.
func init() {
	eeFeatures = append(eeFeatures, func(h *eeHooks, _ *vault.Vault) { h.patching = patching.New(h.licence) })
}
