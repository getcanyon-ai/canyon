// Package outboundkeys is Canyon Enterprise's outbound keys (design/EGRESS.md §4, layer 3; contracts/routes.md,
// Egress control, Interception; #2922): the code that writes a credential an app's calls carry, and the code that
// adds it to a call — the interception root and its leaves, the policy's routes, a route's value, a /run's egress.ca
// and placeholders, and egress-control's interception listener and values. The core reaches it through its hooks
// alone (design/ARCHITECTURE.md §8): the server's eeHooks.outbound, runs.Pipeline.Inject and egresscontrol.Injector.
package outboundkeys

import (
	"context"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/internal/enterprise"
	"github.com/getcanyon-ai/canyon/vault"
)

// Feature is the licence's name for outbound keys.
const Feature = "outbound_keys"

// licence is what Keys asks of the licence: whether one in force unlocks a feature, enterprise.Required when none
// does.
type licence interface {
	Require(ctx context.Context, q enterprise.Querier, feature string) error
}

// Keys is outbound keys for one server: the vault its values and roots are sealed under, its licence, and the
// intermediate its leaves are minted under, in its memory alone.
type Keys struct {
	keys    *vault.Vault
	licence licence
	chain   intermediates
}

// New is outbound keys over keys and lic. egress-control's, which seals and opens nothing, takes neither.
func New(keys *vault.Vault, lic licence) *Keys {
	return &Keys{keys: keys, licence: lic}
}

// egressActor is Canyon, as the trail names it when it rotates its own interception root.
var egressActor = events.Actor{Kind: "canyon", Subject: "egress", Display: "Canyon"}

// trustVariables are the variables that carry an app's own trust, which would replace the roots Canyon adds a
// credential under (contracts/executor.md, egress.ca).
var trustVariables = []string{"NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "SSL_CERT_DIR"}

// envWord is an env as a person reads it: preview, or production.
func envWord(env string) string {
	if env == "prod" {
		return "production"
	}
	return env
}
