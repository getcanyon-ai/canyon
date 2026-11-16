//go:build !noee

package main

import (
	"context"
	"log"

	"github.com/getcanyon-ai/canyon/ee/outboundkeys"
	"github.com/getcanyon-ai/canyon/ee/outboundkeys/intercept"
	"github.com/getcanyon-ai/canyon/internal/egresscontrol"
	"github.com/getcanyon-ai/canyon/vault"
)

func init() {
	eeFeatures = append(eeFeatures, func(h *eeHooks, keys *vault.Vault) {
		h.outbound, h.injector = outboundkeys.New(keys, h.licence), interceptor{}
	})
}

// interceptor makes egress-control's interception, ee/outboundkeys/intercept's.
type interceptor struct{}

func (interceptor) Injector(leafOf func(context.Context, string) ([]byte, error), valueAsk func(context.Context, string, string, string) ([]byte, error), token string, logger *log.Logger) egresscontrol.Injector {
	return intercept.New(intercept.Config{LeafOf: leafOf, ValueAsk: valueAsk, Token: token, Log: logger})
}
